package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/gatewaylaunch"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func gatewayIssuer(t *testing.T) *authissuer.Issuer {
	t.Helper()
	dir := t.TempDir()
	keys, err := authissuer.OpenKeyStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return authissuer.New("gateway-test", keys, state)
}

func TestLocalGatewayCredentialUsesVerifiedSessionCeiling(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux tmpfs")
	}
	svc, _, id, _ := credentialLaunch(t)
	svc.AgentTokens = gatewayIssuer(t)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "parent", Scopes: []string{"session.write"}})
	launchToken, err := svc.mintSessionCredential(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	path, stop, err := svc.startGatewayCredential("https://mcp.example/mcp", id, launchToken, "synthetic-run")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	bearer, err := tokenfile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	issuer := svc.AgentTokens.(*authissuer.Issuer)
	verifier, err := authissuer.NewVerifier(identity.NewStore(svc.Store.DB()), issuer)
	if err != nil {
		t.Fatal(err)
	}
	p, err := verifier.Verify(ctx, bearer)
	if err != nil || p.Kind != "agent" || p.SessionID != id || len(p.Scopes) != 1 || p.Scopes[0] != "tether.sessions" {
		t.Fatal("gateway JWT lost launch ceiling", err)
	}
	if p.ExpiresAt == nil || time.Until(*p.ExpiresAt) > authissuer.DefaultTTL {
		t.Fatal("JWT not short lived")
	}
	if _, _, err := svc.startGatewayCredential("https://mcp.example/mcp", "foreign-session", launchToken, "synthetic-run"); err == nil {
		t.Fatal("foreign session accepted")
	}
	if err := identity.NewStore(svc.Store.DB()).RevokeToken(ctx, launchToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.startGatewayCredential("https://mcp.example/mcp", id, launchToken, "synthetic-run"); err == nil {
		t.Fatal("revoked launch credential accepted")
	}
}

// Exercises the real provider-specific planters, including agy's stdio entry;
// no untested environment interpolation is required by the native CLI.
func TestGatewayEntryPlantedForEveryProviderWithoutCredentials(t *testing.T) {
	for _, brand := range []string{"claude", "codex", "antigravity"} {
		t.Run(brand, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			svc := &Service{CatalogRoot: t.TempDir(), Catalog: &config.Catalog{Global: config.Global{Version: "test"}}}
			ws := t.TempDir()
			mode := config.RuntimeKindPTY
			if brand == "codex" {
				mode = config.RuntimeKindJSONRPCStdio
			}
			if brand == "antigravity" {
				mode = config.RuntimeKindSubprocess
			}
			plan := &launch.Plan{LaunchID: "gateway", ProviderID: brand, ProviderBrand: brand, RuntimeKind: mode, RepoRoot: t.TempDir(), WriteHome: ws, WorkspaceMode: "shared", Command: brand}
			server := gatewaylaunch.Server("/opt/tether", "https://mcp.example/mcp", "/dev/shm/tether-gateway-synthetic/gateway.token")
			admission := testArtifactAdmission(t, plan)
			prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{ArtifactAdmission: admission, GatewayServer: &server})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			if err := filepath.WalkDir(prepared.PlantedBootDir, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if d.Name() != ".mcp.json" && d.Name() != "config.toml" && d.Name() != "mcp_config.json" {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				text := string(data)
				if strings.Contains(text, "mcp-http-relay") && strings.Contains(text, "gateway.token") && strings.Contains(text, "https://mcp.example/mcp") {
					found = true
				}
				for _, bad := range []string{"Authorization", "X-Run-Token", "${TETHER_RUN_TOKEN}", "Bearer "} {
					if strings.Contains(text, bad) {
						t.Error("credential embedded in provider config")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("gateway relay missing from provider config")
			}
		})
	}
}

type shortGatewayIssuer struct {
	authissuer.Issuer
	count atomic.Int32
}

func (*shortGatewayIssuer) RegisterSessionGrant(string, string, map[string][]string, time.Time) error {
	return nil
}
func (*shortGatewayIssuer) RevokeSession(string) error { return nil }
func (s *shortGatewayIssuer) MintLocalSession(p identity.Principal) (authissuer.Token, error) {
	if !p.Verified || p.SessionID == "" {
		return authissuer.Token{}, fmt.Errorf("unverified session")
	}
	return authissuer.Token{Token: fmt.Sprintf("synthetic-lease-%d", s.count.Add(1)), ExpiresAt: time.Now().Add(250 * time.Millisecond)}, nil
}

func TestLocalGatewayRefreshReverifiesLaunchCredentialAndRemovesExpiredLease(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux tmpfs")
	}
	svc, _, id, _ := credentialLaunch(t)
	issuer := &shortGatewayIssuer{}
	svc.AgentTokens = issuer
	launchToken, err := svc.mintSessionCredential(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	path, stop, err := svc.startGatewayCredential("https://mcp.example/mcp", id, launchToken, "synthetic-run")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	initial, err := tokenfile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	rotated := false
	for time.Now().Before(deadline) {
		current, err := tokenfile.Read(path)
		if err == nil && current != initial {
			rotated = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !rotated {
		t.Fatal("local launcher credential was not refreshed")
	}
	if err := identity.NewStore(svc.Store.DB()).RevokeToken(context.Background(), launchToken); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("revoked launcher credential continued refreshing")
}

func TestGatewayOptInRefusesAnOldWorkerBeforeDispatch(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	plan := &launch.Plan{LaunchID: "gateway", LogicalAgentID: "gateway-agent", ProviderID: "claude", ProviderBrand: "claude", Env: map[string]string{gatewaylaunch.URLenv: "https://mcp.example/mcp"}}
	row := store.SessionRow{ID: "old-worker", WorkerID: r.worker, LaunchID: "gateway", LogicalAgentID: plan.LogicalAgentID, ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created"}
	if err := r.db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.launchWorkerSession(context.Background(), &row, plan); err == nil || !strings.Contains(err.Error(), "standalone gateway") {
		t.Fatal("old worker was not refused", err)
	}
	select {
	case <-r.specs:
		t.Fatal("opt-in dispatched to unsupported worker")
	default:
	}
}

func TestGatewayOptInLaunchBindsRealCredentialsWithoutPersistingThem(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux tmpfs")
	}
	svc, _, id, ws := credentialLaunch(t)
	svc.AgentTokens = gatewayIssuer(t)
	plan, err := svc.Store.GetLaunchPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	plan.Env[gatewaylaunch.URLenv] = "https://mcp.example/mcp"
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec("UPDATE launch_plans SET plan_json=? WHERE session_id=?", string(raw), id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	policy, err := svc.Store.SessionMCPPolicy(context.Background(), id)
	if err != nil || slices.Contains(policy.Servers, "shared-services") {
		t.Fatal("gateway opt-in changed upstream grant", err)
	}
	data, err := os.ReadFile(findPlanted(t, filepath.Join(ws, "boot"), ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	args := config.Servers[gatewaylaunch.Name].Args
	var jwtPath, runPath string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--token-file":
			jwtPath = args[i+1]
		case "--run-token-file":
			runPath = args[i+1]
		}
	}
	if jwtPath == "" || runPath == "" {
		t.Fatal("gateway launch did not hand off both credentials")
	}
	bearer, err := tokenfile.Read(jwtPath)
	if err != nil {
		t.Fatal(err)
	}
	runToken, err := tokenfile.Read(runPath)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authissuer.NewVerifier(nil, svc.AgentTokens.(*authissuer.Issuer))
	if err != nil {
		t.Fatal(err)
	}
	p, err := verifier.Verify(context.Background(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyRunToken(runToken, p); err != nil {
		t.Fatal("run token does not bind minted gateway JWT", err)
	}
	if err := filepath.WalkDir(ws, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(bearer)) || bytes.Contains(content, []byte(runToken)) {
			return fmt.Errorf("gateway secret persisted in workspace")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Manager.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Dir(jwtPath)); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("launcher retained gateway handoff after session completion")
}
