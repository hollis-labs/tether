package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/store"
)

type nativeOnlyAdmissionRuntime struct {
	agentsessions.Runtime
	child *strictNativeCoauthorSession
	check func(agentsessions.StartOptions)
}

func (r nativeOnlyAdmissionRuntime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{JsonRpcStdio: true, ProviderSessionID: true}
}
func (r nativeOnlyAdmissionRuntime) Kind() string { return "jsonrpc-stdio" }
func (r nativeOnlyAdmissionRuntime) Start(_ context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	r.check(opts)
	return r.child, nil
}

func TestNativeOnlyPublicResumePreservesContextThroughArtifactAdmission(t *testing.T) {
	ctx := context.Background()
	r := newCodexRig(t)
	credentialHome := prepareStrictNativeCoauthorCredentialHome(t)
	native := t.TempDir()
	if err := os.Mkdir(filepath.Join(native, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "sessions", "retained"), []byte("synthetic native state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(credentialHome, "auth.json"), filepath.Join(native, "auth.json")); err != nil {
		t.Fatal(err)
	}
	provider := r.svc.Catalog.Providers["codex-cli"]
	provider.RuntimeKind = config.RuntimeKindJSONRPCStdio
	r.svc.Catalog.Providers[provider.ID] = provider
	plan, err := launch.Resolve(r.svc.Catalog, launch.Input{LaunchID: "codex-launch", CatalogRoot: r.svc.CatalogRoot})
	if err != nil {
		t.Fatal(err)
	}
	plan.NativeStateRoot = native
	plan.WorkRoot = filepath.Join(t.TempDir(), "missing-historical-repo")
	if err := r.svc.Store.CreateSession(store.SessionRow{ID: "source", LaunchID: "codex-launch", LogicalAgentID: "agent", ProviderID: provider.ID, Workspace: t.TempDir(), State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	policy, err := sessionMCPPolicy("source", "agent", plan, r.svc.Catalog.Global.MCP)
	if err != nil {
		t.Fatal(err)
	}
	policy.UpstreamOwnership, err = r.svc.mcpOwnership()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.SaveSessionMCPPolicy(ctx, policy.Seal()); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.UpsertSessionProviderMapping("source", "tether", provider.ID, "thread-old"); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.SetLogicalAgentLaunchID("agent", "codex-launch"); err != nil {
		t.Fatal(err)
	}
	ids := identity.NewStore(r.svc.Store.DB())
	expiry := time.Now().UTC().Add(time.Hour)
	sourceToken, err := ids.Mint(ctx, identity.Principal{ID: "msg://session/local/source", Kind: "session", SessionID: "source", Scopes: []string{"session.write", "message.write"}, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.RevokeToken(ctx, sourceToken); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.UpdateSessionState("source", "failed", 0, nil); err != nil {
		t.Fatal(err)
	}
	operator, err := ids.Mint(ctx, identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := ids.Verify(ctx, operator)
	if err != nil {
		t.Fatal(err)
	}
	ctx = identity.WithPrincipal(ctx, principal)
	child := &strictNativeCoauthorSession{recoveryFakeSession: &recoveryFakeSession{done: make(chan struct{})}}
	t.Cleanup(func() { _ = child.Stop(context.Background()) })
	base, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	r.svc.resumeGrace = 5 * time.Millisecond
	r.svc.factories[provider.ID] = func(*launch.Plan) (agentsessions.Runtime, error) {
		return nativeOnlyAdmissionRuntime{Runtime: base, child: child, check: func(opts agentsessions.StartOptions) {
			starts++
			if opts.Workdir != native || opts.SessionIDPreset != "thread-old" || opts.AutoFireFirstTurn || len(opts.FirstTurnPayload) != 0 {
				t.Fatal("prepared admission changed retained context")
			}
			home := ""
			for _, entry := range opts.Env {
				if len(entry) > 11 && entry[:11] == "CODEX_HOME=" {
					home = entry[11:]
				}
			}
			if home != native {
				t.Fatal("runtime redirected retained native home")
			}
		}}, nil
	}
	result, err := r.svc.ResumeLogicalAgentWithContext(ctx, "agent", api.ResumeOptions{NativeOnly: true, SourceSessionID: "source", ResumeWorkRoot: native, IdempotencyKey: "strict-context"})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID == "source" || result.SessionID == "" || starts != 1 {
		t.Fatal("canonical identity or native launch incorrect")
	}
	accepted, err := r.svc.Store.GetLaunchPlan(result.SessionID)
	if err != nil || !accepted.NativeResumeOnly || accepted.NativeResumeWorkRoot != native || accepted.NativeStateRoot != native {
		t.Fatal("accepted provenance not persisted", err)
	}
	original, err := r.svc.Store.GetLaunchPlan("source")
	if err != nil || original.WorkRoot != plan.WorkRoot {
		t.Fatal("historical workroot rewritten", err)
	}
	replay, err := r.svc.ResumeLogicalAgentWithContext(ctx, "agent", api.ResumeOptions{NativeOnly: true, SourceSessionID: "source", ResumeWorkRoot: native, IdempotencyKey: "strict-context"})
	if err != nil || replay.SessionID != result.SessionID || !replay.Replayed || starts != 1 {
		t.Fatal("strict retry allocated or relaunched context", err)
	}
}
