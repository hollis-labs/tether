package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDeviceRenewListRevokeAndAudit(t *testing.T) {
	s, db := identityStore(t)
	ctx := context.Background()
	grant, err := s.CreatePairingGrant(ctx, pairingOperator(), "synthetic worker", []string{"read", "operate"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.ExchangePairingGrant(ctx, grant.Code, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Verify(ctx, device.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), []string{"admin"}); err == nil {
		t.Fatal("renew widened scopes")
	}
	if _, _, err := s.RenewDevice(ctx, p, strings.Repeat("0", 64), nil); err == nil {
		t.Fatal("renew ignored current credential")
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), []string{"read"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), nil); err == nil {
		t.Fatal("stale principal restored removed scope")
	}
	p, err = s.Verify(ctx, device.Token)
	if err != nil || len(p.Scopes) != 1 {
		t.Fatal("narrowing not persisted", err)
	}
	o := identity.Observation{At: time.Now(), Authentication: "verified", Method: "GET", Route: "/sessions"}
	if err := s.RecordDeviceUse(ctx, p, o, "192.0.2.7:7331", "synthetic-worker/1"); err != nil {
		t.Fatal(err)
	}
	devices, err := s.ListDevices(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatal("device list failed", err)
	}
	if devices[0].ID != p.ID || devices[0].LastUsedAt == nil || devices[0].RemoteAddress != "192.0.2.7" || devices[0].UserAgent != "synthetic-worker/1" {
		t.Fatal("metadata not tied to device")
	}
	var audited string
	if err := db.DB().QueryRow(`SELECT principal_id FROM identity_audit WHERE route='/sessions'`).Scan(&audited); err != nil || audited != p.ID {
		t.Fatal("missing device use audit", err)
	}
	if err := s.RecordDeviceUse(ctx, p, o, "forged.invalid:1", device.Token); err != nil {
		t.Fatal(err)
	}
	devices, err = s.ListDevices(ctx)
	if err != nil || devices[0].UserAgent != "<redacted>" || devices[0].RemoteAddress != "" {
		t.Fatal("unsafe metadata retained", err)
	}
	if err := s.RevokeDevice(ctx, identity.OperatorID); !errors.Is(err, identity.ErrDeviceNotFound) {
		t.Fatal("device API revoked operator", err)
	}
	if err := s.RevokeDevice(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, device.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("revoked device still valid", err)
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), nil); err == nil {
		t.Fatal("revoked renewal accepted")
	}
	devices, err = s.ListDevices(ctx)
	if err != nil || devices[0].RevokedAt == nil {
		t.Fatal("revocation missing from list", err)
	}
}

type deviceNativeResumeAPI struct {
	api.LaunchService
	service  *app.Service
	callerID string
}

func (a *deviceNativeResumeAPI) ResumeLogicalAgentWithContext(ctx context.Context, id string, opts api.ResumeOptions) (api.LaunchResult, error) {
	caller, _ := identity.FromContext(ctx)
	a.callerID = caller.ID
	result, err := a.service.ResumeLogicalAgentWithContext(ctx, id, opts)
	if !errors.Is(err, app.ErrNativeOnlyUnavailable) {
		return result, errors.New("synthetic strict native refusal witness did not reach the credential ceiling")
	}
	return result, err
}

// These witnesses exercise existing execution authority, rather than granting
// legacy worker scopes from the independent device route scopes.
func TestDeviceOperatePreservesNativeOnlyRefusal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ids, db := identityStore(t)
	ctx := context.Background()
	credentialHome, native, work := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(credentialHome, "auth.json"), []byte(`{"synthetic_fixture":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", credentialHome)
	if err := os.Mkdir(filepath.Join(native, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "sessions", "retained"), []byte("synthetic native state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(credentialHome, "auth.json"), filepath.Join(native, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertLogicalAgent(agent.LogicalAgent{ID: "agent"}, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetLogicalAgentLaunchID("agent", "synthetic"); err != nil {
		t.Fatal(err)
	}
	source := &launch.Plan{LaunchID: "synthetic", LogicalAgentID: "agent", ProviderID: "synthetic-codex", ProviderBrand: "codex", WorkRoot: work, NativeStateRoot: native}
	if err := db.CreateSession(store.SessionRow{ID: "source", LaunchID: "synthetic", LogicalAgentID: "agent", ProviderID: source.ProviderID, State: "created"}, source); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(time.Hour)
	if _, err := ids.Mint(ctx, identity.Principal{ID: "msg://session/local/source", Kind: "session", SessionID: "source", Scopes: []string{"session.write", "message.write"}, ExpiresAt: &expiry}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionState("source", "failed", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSessionProviderMapping("source", "tether", source.ProviderID, "retained-thread"); err != nil {
		t.Fatal(err)
	}
	// No runtime factory or Manager exists: refusal precedes allocation/execution.
	svc := &app.Service{Store: db, Catalog: &config.Catalog{
		Global:    config.Global{Version: "test", Identity: config.IdentityConfig{Mode: "enforce"}},
		Projects:  map[string]config.Project{"project": {ID: "project", RepoRoot: work, Workspace: config.WorkspaceSpec{SessionRoot: t.TempDir(), DefaultMode: "shared"}}},
		Agents:    map[string]config.Agent{"agent": {ID: "agent"}},
		Providers: map[string]config.Provider{source.ProviderID: {ID: source.ProviderID, Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio, Command: "synthetic-unused"}},
		Launches:  map[string]config.Launch{"synthetic": {ID: "synthetic", Project: "project", Agent: "agent", Provider: source.ProviderID}},
	}}

	for _, scopes := range [][]string{{"operate"}, {"read", "operate", "terminal", "maintain", "admin"}} {
		grant, err := ids.CreatePairingGrant(ctx, pairingOperator(), "synthetic native client", scopes, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		device, err := ids.ExchangePairingGrant(ctx, grant.Code, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		adapter := &deviceNativeResumeAPI{service: svc}
		handler := identity.RemoteMiddleware(ids, nil, api.RemoteScopeMiddleware(api.NewHandler(api.Deps{Service: adapter})))
		request := httptest.NewRequest(http.MethodPost, "/logical-agents/agent/resume", strings.NewReader(`{"native_only":true,"source_session_id":"source"}`))

		request.Header.Set("Authorization", "Bearer "+device.Token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if adapter.callerID != device.Principal.ID || response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "independent current launch authority required") {
			t.Fatalf("operate route did not reach strict execution refusal: %d", response.Code)
		}
	}
	latest, err := db.LatestSessionForAgent(ctx, "agent")
	if err != nil || latest.ID != "source" {
		t.Fatal("refusal allocated destination", err)
	}
	var destination, credential string
	if err := db.DB().QueryRow(`SELECT COALESCE(MAX(id),'') FROM sessions WHERE id <> 'source'`).Scan(&destination); err != nil || destination != "" {
		t.Fatal("refusal allocated a new session", err)
	}
	if err := db.DB().QueryRow(`SELECT COALESCE(MAX(principal_id),'') FROM principals WHERE kind='session' AND session_id <> 'source'`).Scan(&credential); err != nil || credential != "" {
		t.Fatal("refusal minted a child credential", err)
	}

	mapping, err := db.GetSessionProviderMapping("source", "tether", source.ProviderID)
	if err != nil || mapping.NativeSessionID.String != "retained-thread" {
		t.Fatal("refusal changed retained native mapping", err)
	}
}

func TestDeviceChildCredentialDoesNotInheritLegacyScopes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "providers"), 0700); err != nil {
		t.Fatal(err)
	}
	global := "version: 0.1.0\nidentity:\n  mode: enforce\ncatalog:\n  defaults:\n    state_db: " + filepath.Join(root, "state.db") + "\n"
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte(global), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "providers", "stub.yaml"), []byte("id: api-stub\ntype: api\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// api-stub runs entirely in-process; no provider binary or daemon is used.
	svc, err := app.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := svc.Manager.Stop(cleanup, "child"); err != nil {
			t.Error(err)
		}
		if _, err := svc.Manager.WaitSession(cleanup, "child"); err != nil {
			t.Error(err)
		}
		if err := svc.Close(); err != nil {
			t.Error(err)
		}
	})
	// The fixture exercises child credentials, with no registry bindings.
	svc.Registry = nil
	ids := identity.NewStore(svc.Store.DB())
	ctx := context.Background()
	grant, err := ids.CreatePairingGrant(ctx, pairingOperator(), "synthetic parent", []string{"read", "operate", "terminal", "maintain", "admin"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	device, err := ids.ExchangePairingGrant(ctx, grant.Code, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	parentExpiry := time.Now().UTC().Add(30 * time.Minute)
	if _, err := svc.Store.DB().Exec(`UPDATE principals SET expires_at=? WHERE principal_id=?`, parentExpiry.Format("2006-01-02T15:04:05.000000000Z"), device.Principal.ID); err != nil {
		t.Fatal(err)
	}
	parent, err := ids.Verify(ctx, device.Token)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	plan := &launch.Plan{LaunchID: "synthetic", ProviderID: "api-stub", ProviderBrand: "claude", RuntimeKind: config.RuntimeKindAPI, RepoRoot: t.TempDir(), WriteHome: workspace, WorkspaceMode: "shared", Command: "synthetic-unused"}
	if err := svc.Store.CreateSession(store.SessionRow{ID: "child", Workspace: workspace, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	result, err := svc.LaunchSessionWithContext(identity.WithPrincipal(ctx, parent), "child")
	if err != nil || result == nil {
		t.Fatal("synthetic child launch failed", err)
	}
	var raw, creator, kind, expiry string
	if err := svc.Store.DB().QueryRow(`SELECT scopes_json,created_by,kind,expires_at FROM principals WHERE session_id='child' AND revoked_at IS NULL`).Scan(&raw, &creator, &kind, &expiry); err != nil {
		t.Fatal(err)
	}
	var scopes []string
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 0 || creator != parent.ID || kind != "session" {
		t.Fatal("device child inherited legacy execution or MCP scope")
	}
	childExpiry, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil || !childExpiry.After(parentExpiry) {
		t.Fatal("ordinary child parent-expiry limitation changed", err)
	}
	if err := ids.RevokeDevice(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ids.Verify(ctx, device.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("synthetic parent revoke failed", err)
	}
	var activeChild string
	if err := svc.Store.DB().QueryRow(`SELECT principal_id FROM principals WHERE session_id='child' AND revoked_at IS NULL`).Scan(&activeChild); err != nil || activeChild != "msg://session/local/child" {
		t.Fatal("ordinary child parent-revocation limitation changed", err)
	}

}
