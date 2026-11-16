package app

import (
	"context"
	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/workerwire"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
)

func TestWorkerNativePolicyRequiresCurrentExactAuthorization(t *testing.T) {
	for _, tc := range []string{"approved-change", "missing-digest", "stale-digest", "session-writer-only", "revoked-caller", "expired-source", "old-policy-corrupt", "unverified"} {
		t.Run(tc, func(t *testing.T) {
			s := bindingHarness(t)
			ctx := context.Background()
			p := &launch.Plan{LogicalAgentID: "agent", ResumeSourceSessionID: "source", NativeResumeOnly: true}
			if err := s.Store.CreateSession(store.SessionRow{ID: "source", LogicalAgentID: "agent", State: "created"}, p); err != nil {
				t.Fatal(err)
			}
			old := mcpgateway.SessionPolicy{SessionID: "source", AgentID: "agent", Servers: []string{"old"}}.Seal()
			if err := s.Store.SaveSessionMCPPolicy(ctx, old); err != nil {
				t.Fatal(err)
			}

			if err := s.Store.CreateSession(store.SessionRow{ID: "new", LogicalAgentID: "agent", State: "created"}, p); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := s.Store.DB().QueryRow(`SELECT policy_json FROM session_mcp_policy WHERE session_id='source'`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			current := mcpgateway.SessionPolicy{SessionID: "new", AgentID: "agent", Servers: []string{"old", "reader"}}.Seal()
			review := current
			review.SessionID = "source"
			p.WorkerPolicyDigest = review.Seal().Digest
			ids := identity.NewStore(s.Store.DB())
			expiry := time.Now().Add(time.Hour)

			if _, err := ids.Mint(ctx, identity.Principal{ID: "msg://session/local/source", Kind: "session", SessionID: "source", Scopes: []string{"session.write"}, ExpiresAt: &expiry}); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.UpdateSessionState("source", "failed", 0, nil); err != nil {
				t.Fatal(err)
			}
			if tc == "expired-source" {
				_, err := s.Store.DB().Exec(`UPDATE principals SET expires_at=? WHERE session_id='source'`, time.Now().Add(-time.Hour).Format(time.RFC3339Nano))
				if err != nil {
					t.Fatal(err)
				}
			}
			scopes := []string{"*"}
			if tc == "session-writer-only" {
				scopes = []string{"session.write"}
			}
			token, err := ids.Mint(ctx, identity.Principal{ID: "reviewer", Kind: "operator", Scopes: scopes})
			if err != nil {
				t.Fatal(err)
			}
			caller, err := ids.Verify(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "missing-digest":
				p.WorkerPolicyDigest = ""
			case "stale-digest":
				p.WorkerPolicyDigest = old.Digest
			case "revoked-caller":
				if err := ids.RevokeToken(ctx, token); err != nil {
					t.Fatal(err)
				}
			case "unverified":
				caller.Verified = false
			case "old-policy-corrupt":
				_, err = s.Store.DB().Exec(`UPDATE session_mcp_policy SET policy_json='{}' WHERE session_id='source'`)
				if err != nil {
					t.Fatal(err)
				}
			}
			err = s.workerNativePolicy(identity.WithPrincipal(ctx, caller), p, current)
			if tc == "approved-change" {
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Store.SaveSessionMCPPolicy(ctx, current); err != nil {
					t.Fatal(err)
				}
				var after string
				_ = s.Store.DB().QueryRow(`SELECT policy_json FROM session_mcp_policy WHERE session_id='source'`).Scan(&after)
				if after != before {
					t.Fatal("source policy mutated")
				}
			} else if err == nil {
				t.Fatal("unsafe authority accepted")
			}
		})
	}
}

func TestWorkerNativeContinuationPublicAdmission(t *testing.T) {
	r := newIdentityRig(t, workerwire.FeatureCodexResume, workerwire.FeatureWorkerResume, workerwire.FeatureProviderProcess, workerwire.FeatureSessionCred)
	s := r.svc
	s.WorkerResume = true
	s.CatalogRoot = t.TempDir()
	s.Catalog.Projects = map[string]config.Project{"p": {ID: "p", RepoRoot: t.TempDir(), Workspace: config.WorkspaceSpec{SessionRoot: t.TempDir(), DefaultMode: "shared"}}}
	s.Catalog.Agents = map[string]config.Agent{"portfolio-manager": {ID: "portfolio-manager", URN: r.urn}}
	s.Catalog.Providers = map[string]config.Provider{"codex-app-server": {ID: "codex-app-server", Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio, Command: "codex"}}
	s.Catalog.Launches = map[string]config.Launch{"seat": {ID: "seat", Project: "p", Agent: "portfolio-manager", Provider: "codex-app-server"}}
	s.Catalog.Global.Catalog.Defaults.WorkspaceRoot = t.TempDir()
	plan, err := launch.Resolve(s.Catalog, launch.Input{LaunchID: "seat", CatalogRoot: s.CatalogRoot})
	if err != nil {
		t.Fatal(err)
	}
	s.applyCatalogIdentity(plan)
	source := store.SessionRow{ID: "native-source", LaunchID: "seat", ProjectID: "p", LogicalAgentID: "portfolio-manager", ProviderID: "codex-app-server", WorkerID: r.worker, State: "created", Workspace: t.TempDir()}
	if err := r.db.CreateSession(source, plan); err != nil {
		t.Fatal(err)
	}
	policy, err := sessionMCPPolicy(source.ID, source.LogicalAgentID, plan, s.Catalog.Global.MCP)
	if err != nil {
		t.Fatal(err)
	}
	policy.UpstreamOwnership, err = s.mcpOwnership()
	if err != nil {
		t.Fatal(err)
	}
	policy = policy.Seal()
	if err := r.db.SaveSessionMCPPolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	oldToken, err := r.ids.Mint(context.Background(), identity.Principal{ID: "msg://session/local/" + source.ID, Kind: "session", SessionID: source.ID, Scopes: []string{"*"}, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ids.RevokeToken(context.Background(), oldToken); err != nil {
		t.Fatal(err)
	}
	if err := r.db.UpdateSessionState(source.ID, "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.db.UpsertLogicalAgent(agent.LogicalAgent{ID: source.LogicalAgentID, Name: source.LogicalAgentID}, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := r.db.SetLogicalAgentLaunchID(source.LogicalAgentID, "seat"); err != nil {
		t.Fatal(err)
	}
	caller, err := r.ids.Verify(context.Background(), r.op)
	if err != nil {
		t.Fatal(err)
	}
	ctx := identity.WithPrincipal(context.Background(), caller)
	options := api.ResumeOptions{NativeOnly: true, SourceSessionID: source.ID, WorkerSourceGeneration: "generation-1", WorkerPolicyDigest: policy.Digest}
	bad := options
	bad.WorkerSourceGeneration = "stale"
	if _, err := s.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, bad); err == nil {
		t.Fatal("stale source accepted")
	}
	latest, err := r.db.LatestSessionForAgent(ctx, source.LogicalAgentID)
	if err != nil || latest.ID != source.ID {
		t.Fatal("refused probe allocated a child")
	}
	result, err := s.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, options)
	if err != nil {
		t.Fatal(err)
	}
	child, err := r.db.GetSession(result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if child.WorkerID != source.WorkerID || child.ParentSessionID.String != source.ID || child.Intent != "resume" || child.State != "running" {
		t.Fatalf("wrong child %+v", child)
	}
	mapping, err := r.db.GetSessionProviderMapping(child.ID, "tether", source.ProviderID)
	if err != nil || mapping.NativeSessionID.String != "thread-retained" {
		t.Fatalf("mapping %+v %v", mapping, err)
	}
	spec := <-r.specs
	if spec.ResumeSessionID != "thread-retained" || spec.ResumeGeneration != "generation-1" || spec.BootPrompt != "" || spec.AgentURN != r.urn {
		t.Fatalf("wrong continuation spec %+v", spec)
	}
	saved, err := r.db.GetLaunchPlan(child.ID)
	if err != nil || saved.WorkRoot != "/worker/retained" {
		t.Fatalf("workroot %+v %v", saved, err)
	}
	var raw string
	_ = r.db.DB().QueryRow(`SELECT policy_json FROM session_mcp_policy WHERE session_id=?`, source.ID).Scan(&raw)
	var retained mcpgateway.SessionPolicy
	if err := decodeWorkerSourcePolicy(raw, &retained); err != nil || retained.Digest != policy.Digest {
		t.Fatal("old policy changed")
	}
}

func TestWorkerNativeRetryOnlyUnadmittedFailedChild(t *testing.T) {
	for _, state := range []string{"failed", "running", "completed", "unrelated", "native-mapped"} {
		t.Run(state, func(t *testing.T) {
			s := bindingHarness(t)
			source := store.SessionRow{ID: "source", LogicalAgentID: "agent", WorkerID: "w", ProviderID: "codex", State: "completed"}
			if err := s.Store.CreateSession(source, &launch.Plan{}); err != nil {
				t.Fatal(err)
			}
			plan := &launch.Plan{NativeResumeOnly: true, WorkerSourceGeneration: "g", WorkerPolicyDigest: "p"}
			latest := store.SessionRow{ID: "failed-child", LogicalAgentID: "agent", WorkerID: "w", ProviderID: "codex", State: "failed"}
			latest.ParentSessionID.String = "source"
			latest.ParentSessionID.Valid = true
			if state == "running" || state == "completed" {
				latest.State = state
			}
			if state == "unrelated" {
				latest.ParentSessionID.String = "other"
			}
			if err := s.Store.CreateSession(latest, plan); err != nil {
				t.Fatal(err)
			}
			if state == "native-mapped" {
				if err := s.Store.UpsertSessionProviderMapping(latest.ID, "tether", "codex", "thread"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.retryWorkerNativeSource(&latest, api.ResumeOptions{SourceSessionID: "source", WorkerSourceGeneration: "g", WorkerPolicyDigest: "p"})
			if state == "failed" {
				if err != nil || got.ID != "source" {
					t.Fatalf("retry refused %+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("unsafe retry accepted")
			}
		})
	}
}
