package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/store"
)

type credentialRuntime struct {
	agentsessions.Runtime
	options agentsessions.StartOptions
	fail    bool
}

func (r *credentialRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	r.options = opts
	if r.fail {
		return nil, errors.New("test start failure")
	}
	return r.Runtime.Start(ctx, opts)
}
func credentialLaunch(t *testing.T, brands ...string) (*Service, *credentialRuntime, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ws := t.TempDir()
	plan := &launch.Plan{LaunchID: "launch", ProjectID: "project", LogicalAgentID: "agent", ProviderID: "stub", ProviderBrand: "claude", RuntimeKind: config.RuntimeKindPTY,
		RepoRoot: t.TempDir(), WriteHome: ws, WorkspaceMode: "shared", Command: "test-fake", Env: map[string]string{"TETHER_TOKEN": "parent-credential"}}
	if len(brands) > 0 {
		plan.ProviderBrand = brands[0]
		if brands[0] == "codex" {
			plan.RuntimeKind = config.RuntimeKindJSONRPCStdio
		}
	}
	id := "session-test"
	if err := db.CreateSession(store.SessionRow{ID: id, LogicalAgentID: "agent", Workspace: ws, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	rt, _ := stub.New(plan)
	wrapped := &credentialRuntime{Runtime: rt}
	svc := &Service{Store: db, CatalogRoot: t.TempDir(), Catalog: &config.Catalog{Global: config.Global{Version: "test"}},
		Manager: agentsessions.NewManager(stateSinkAdapter{db: db}), factories: map[string]RuntimeFactory{"stub": func(_ *launch.Plan) (agentsessions.Runtime, error) { return wrapped, nil }}}
	t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), id) })
	return svc, wrapped, id, ws
}
func TestSessionCredentialLaunchAndTerminalRevocation(t *testing.T) {
	for _, brand := range []string{"claude", "codex"} {
		t.Run(brand, func(t *testing.T) { testSessionCredentialLaunchAndTerminalRevocation(t, brand) })
	}
}
func testSessionCredentialLaunchAndTerminalRevocation(t *testing.T, brand string) {
	svc, rt, id, ws := credentialLaunch(t, brand)
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "parent", Scopes: []string{"session.write"}})
	launched, err := svc.LaunchSessionWithContext(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	for _, env := range rt.options.Env {
		if strings.HasPrefix(env, "TETHER_TOKEN=") {
			token = strings.TrimPrefix(env, "TETHER_TOKEN=")
		}
	}
	argv, err := rt.options.Launch.TurnArgv(gop.TurnInput{Prompt: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), token) {
		t.Fatal("raw credential in provider argv")
	}
	ids := identity.NewStore(svc.Store.DB())
	p, err := ids.Verify(ctx, token)
	if err != nil {
		t.Fatal("runtime credential not verified", err)
	}
	if p.SessionID != id || p.CreatedBy != "parent" || !slices.Equal(p.Scopes, []string{"session.write"}) || !slices.Equal(p.Addresses, []string{"msg://session/local/" + id}) {
		t.Fatal("wrong session credential provenance or grants")
	}
	if launched.Plan.Env["TETHER_TOKEN"] != "parent-credential" {
		t.Fatal("mutated returned plan")
	}
	var saved string
	if err := svc.Store.DB().QueryRow(`SELECT plan_json FROM launch_plans WHERE session_id=?`, id).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved, token) {
		t.Fatal("raw token persisted in launch plan")
	}
	var found bool
	err = filepath.WalkDir(filepath.Join(ws, "boot"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != ".mcp.json" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var config struct {
			Servers map[string]struct {
				Args []string          `json:"args"`
				Env  map[string]string `json:"env"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(body, &config); err != nil {
			return err
		}
		for _, server := range config.Servers {
			if !slices.Contains(server.Args, "mcp") {
				continue
			}
			found = true
			if server.Env["TETHER_TOKEN"] != token {
				t.Error("MCP environment lacks runtime credential")
			}
			if slices.Contains(server.Args, "--token") || strings.Contains(strings.Join(server.Args, " "), token) {
				t.Error("credential in argv")
			}
		}
		return nil
	})
	if err != nil || !found {
		t.Fatal("MCP planting missing", err)
	}
	if err := svc.Manager.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Manager.WaitSession(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := ids.Verify(ctx, token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("terminal credential remained usable", err)
	}
	if _, err := ids.Mint(ctx, identity.Principal{ID: "late", Kind: "session", SessionID: id}); err == nil {
		t.Fatal("minted after terminal state")
	}
}
func TestSessionCredentialLaunchFailureRevokes(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	rt.fail = true
	if _, err := svc.LaunchSession(id); err == nil {
		t.Fatal("launch succeeded")
	}
	var revoked *string
	if err := svc.Store.DB().QueryRow(`SELECT revoked_at FROM principals WHERE session_id=?`, id).Scan(&revoked); err != nil || revoked == nil {
		t.Fatal("failed-launch credential not revoked", err)
	}
}
func TestSessionCredentialRevokesAcrossStoreTerminalPaths(t *testing.T) {
	for _, state := range []string{"completed", "failed", "killed", "delete"} {
		t.Run(state, func(t *testing.T) {
			svc, _, id, _ := credentialLaunch(t)
			token, err := svc.mintSessionCredential(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if state == "delete" {
				_, err = svc.Store.DB().Exec(`DELETE FROM sessions WHERE id=?`, id)
			} else {
				err = svc.Store.UpdateSessionState(id, state, 0, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := identity.NewStore(svc.Store.DB()).Verify(context.Background(), token); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatal("credential survived terminal/delete", err)
			}
		})
	}
}

func TestSessionCredentialLaunchReplacesStaleToken(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	ctx := context.Background()
	stale, err := svc.mintSessionCredential(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal("stale credential wedged launch", err)
	}
	ids := identity.NewStore(svc.Store.DB())
	if _, err := ids.Verify(ctx, stale); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("stale token remained valid", err)
	}
	var current string
	for _, entry := range rt.options.Env {
		if strings.HasPrefix(entry, "TETHER_TOKEN=") {
			current = strings.TrimPrefix(entry, "TETHER_TOKEN=")
		}
	}
	if current == stale {
		t.Fatal("launch reused stale credential")
	}
	if err := ids.RevokeToken(ctx, stale); err != nil {
		t.Fatal(err)
	}
	p, err := ids.Verify(ctx, current)
	if err != nil {
		t.Fatal("old cleanup revoked replacement", err)
	}
	if p.ExpiresAt == nil || !p.ExpiresAt.After(time.Now()) {
		t.Fatal("session lacks expiry backstop")
	}
}

func TestSessionCredentialMintFailureAvailability(t *testing.T) {
	for _, mode := range []string{"observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			svc, rt, id, ws := credentialLaunch(t)
			t.Setenv("TETHER_TOKEN", "inherited-operator-secret")
			svc.Catalog.Global.Identity.Mode = mode
			_, err := svc.Store.DB().Exec(`CREATE TRIGGER test_mint_failure BEFORE INSERT ON principals WHEN NEW.kind = 'session' BEGIN SELECT RAISE(ABORT, 'test mint unavailable'); END;`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.LaunchSession(id)
			if mode == "enforce" {
				if err == nil {
					t.Fatal("enforce launch continued without credential")
				}
				if rt.options.Env != nil {
					t.Fatal("enforce started runtime")
				}
				return
			}
			if err != nil {
				t.Fatal("observe mint failure stopped launch", err)
			}
			if !slices.Contains(rt.options.Env, "TETHER_TOKEN=") {
				t.Fatal("inherited runtime credential not cleared")
			}
			for _, entry := range rt.options.Env {
				if strings.HasPrefix(entry, "TETHER_TOKEN=") && entry != "TETHER_TOKEN=" {
					t.Fatal("inherited runtime credential leaked")
				}
			}
			var found bool
			err = filepath.WalkDir(filepath.Join(ws, "boot"), func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || d.Name() != ".mcp.json" {
					return nil
				}
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				var config struct {
					Servers map[string]struct {
						Env map[string]string `json:"env"`
					} `json:"mcpServers"`
				}
				if err := json.Unmarshal(body, &config); err != nil {
					return err
				}
				for _, server := range config.Servers {
					if server.Env["TETHER_MCP_TOKEN"] == "tether-worker" {
						found = true
						if server.Env["TETHER_TOKEN"] != "" {
							t.Fatal("inherited MCP credential leaked")
						}
					}
				}
				return nil
			})
			if err != nil || !found {
				t.Fatal("anonymous MCP environment missing", err)
			}
		})
	}
}

func TestSessionCredentialConcurrentLaunchKeepsWinner(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; _, err := svc.LaunchSession(id); results <- err }()
	}
	close(start)
	first, second := <-results, <-results
	if first != nil {
		first, second = second, first
	}
	if first != nil || !errors.Is(second, ErrSessionNotCreated) {
		t.Fatal("expected one launch and a lifecycle conflict", first, second)
	}
	var token string
	for _, entry := range rt.options.Env {
		if strings.HasPrefix(entry, "TETHER_TOKEN=") {
			token = strings.TrimPrefix(entry, "TETHER_TOKEN=")
		}
	}
	if _, err := identity.NewStore(svc.Store.DB()).Verify(context.Background(), token); err != nil {
		t.Fatal("winning runtime token invalid", err)
	}
	svc.launchMu.Lock()
	defer svc.launchMu.Unlock()
	if len(svc.launches) != 0 {
		t.Fatal("completed launch guard retained session")
	}
}
