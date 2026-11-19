package app

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/workerwire"
	"strings"
	"sync/atomic"
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

func nativeContinuationRig(t *testing.T) (*identityRig, context.Context, store.SessionRow, api.ResumeOptions, mcpgateway.SessionPolicy) {
	t.Helper()
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
	return r, ctx, source, options, policy
}

func TestWorkerNativeContinuationPublicAdmission(t *testing.T) {
	r, ctx, source, options, policy := nativeContinuationRig(t)
	options.IdempotencyPrincipal = "native-requester"
	s := r.svc
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
	if child.RequestingPrincipal.String != "native-requester" || child.WorkerID != source.WorkerID || child.ParentSessionID.String != source.ID || child.Intent != "resume" || child.State != "running" {
		t.Fatalf("wrong child %+v", child)
	}
	mapping, err := r.db.GetSessionProviderMapping(child.ID, "tether", source.ProviderID)
	if err != nil || mapping.NativeSessionID.String != "thread-retained" {
		t.Fatalf("mapping %+v %v", mapping, err)
	}
	receipt, err := s.GetWorkerNativeAdmission(child.ID)
	if err != nil || receipt == nil || receipt.SourceSessionID != source.ID || receipt.SourceGeneration != options.WorkerSourceGeneration || receipt.ReplacementSessionID != child.ID || receipt.ReplacementGeneration != "new-generation" || receipt.NativeThreadID != "thread-retained" || receipt.Workroot != "/worker/retained" {
		t.Fatalf("admission receipt: %+v %v", receipt, err)
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

// The first probe succeeds before the canonical child exists. A refusal at the
// second probe/authority check must leave a retryable failed child, while a
// dropped response after launch dispatch must retain the uncertain writer.
func TestWorkerNativeAdmissionRefusalRecovery(t *testing.T) {
	for _, fault := range []string{"probe-disconnect", "authority-revoked", "launch-response-lost"} {
		t.Run(fault, func(t *testing.T) {
			r, ctx, source, options, _ := nativeContinuationRig(t)
			var probes, launches atomic.Int32
			hook := func(ws *websocket.Conn, f workerwire.Frame) bool {
				if f.T == "launch" {
					launches.Add(1)
					if fault == "launch-response-lost" {
						_ = ws.Close()
						return true
					}
				}
				if f.T == "resume_probe" && probes.Add(1) == 2 {
					switch fault {
					case "probe-disconnect":
						_ = ws.Close()
						return true
					case "authority-revoked":
						if err := r.ids.RevokeToken(context.Background(), r.op); err != nil {
							t.Error(err)
						}
					}
				}
				return false
			}
			r.frameHook.Store(&hook)
			if _, err := r.svc.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, options); err == nil {
				t.Fatal("injected refusal was accepted")
			}
			failed, err := r.db.LatestSessionForAgent(ctx, source.LogicalAgentID)
			if err != nil || failed == nil || failed.ID == source.ID || failed.ParentSessionID.String != source.ID {
				t.Fatalf("missing canonical child: %+v %v", failed, err)
			}
			wantState, wantLaunches := "failed", int32(0)
			if fault == "launch-response-lost" {
				wantState, wantLaunches = "launching", 1
			}
			if failed.State != wantState || launches.Load() != wantLaunches {
				t.Fatalf("state=%s launch frames=%d; want %s/%d", failed.State, launches.Load(), wantState, wantLaunches)
			}
			if _, err := r.db.GetSessionProviderMapping(failed.ID, "tether", source.ProviderID); err == nil {
				t.Fatal("refused/uncertain child acquired native custody")
			}
			if receipt, err := r.svc.GetWorkerNativeAdmission(failed.ID); err != nil || receipt != nil {
				t.Fatalf("refused attempt has admission: %+v %v", receipt, err)
			}
			r.frameHook.Store(nil)
			if fault != "authority-revoked" {
				r.reconnect()
			} else {
				// The original revoked caller must still fail before allocating a retry.
				if _, err := r.svc.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, options); err == nil || !strings.Contains(err.Error(), "authority") {
					t.Fatalf("revoked caller retry: %v", err)
				}
				token, err := r.ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
				if err != nil {
					t.Fatal(err)
				}
				caller, err := r.ids.Verify(context.Background(), token)
				if err != nil {
					t.Fatal(err)
				}
				ctx = identity.WithPrincipal(context.Background(), caller)
			}
			if fault == "launch-response-lost" {
				if _, err := r.svc.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, options); err == nil {
					t.Fatal("uncertain writer permitted another launch")
				}
				latest, _ := r.db.LatestSessionForAgent(ctx, source.LogicalAgentID)
				if latest.ID != failed.ID || latest.State != "launching" {
					t.Fatalf("uncertain attempt changed: %+v", latest)
				}
				return
			}
			bad := options
			bad.WorkerSourceGeneration = "stale"
			if _, err := r.svc.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, bad); err == nil {
				t.Fatal("retry bypassed exact generation")
			}
			result, err := r.svc.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, options)
			if err != nil {
				t.Fatal(err)
			}
			child, err := r.db.GetSession(result.SessionID)
			if err != nil || child.State != "running" || child.ID == failed.ID || child.ParentSessionID.String != source.ID || child.WorkerID != source.WorkerID {
				t.Fatalf("retry child: %+v %v", child, err)
			}
			spec := <-r.specs
			if spec.ResumeSessionID != "thread-retained" || spec.ResumeGeneration != options.WorkerSourceGeneration || spec.ResumeSourceSessionID != source.ID || spec.BootPrompt != "" {
				t.Fatalf("retry lost native context: %+v", spec)
			}
			select {
			case <-r.specs:
				t.Fatal("extra launch frame")
			default:
			}
			old, _ := r.db.GetSession(failed.ID)
			if old.State != "failed" {
				t.Fatalf("historical refusal changed: %+v", old)
			}
		})
	}
}

func TestWorkerNativeAdmissionRejectsWrongReadiness(t *testing.T) {
	for _, field := range []string{"session", "generation", "actor", "thread", "workroot"} {
		t.Run(field, func(t *testing.T) {
			r, ctx, source, options, _ := nativeContinuationRig(t)
			hook := func(ws *websocket.Conn, f workerwire.Frame) bool {
				if f.T != "launch" {
					return false
				}
				receipt := &workerwire.NativeContext{Session: f.Session, Generation: "new-generation", Actor: "msg://agent/portfolio-manager", Thread: "thread-retained", Workroot: "/worker/retained"}
				var spec workerwire.LaunchSpec
				if err := json.Unmarshal(f.Spec, &spec); err != nil {
					t.Error(err)
					return true
				}
				receipt.Actor = spec.Actor
				switch field {
				case "session":
					receipt.Session = "other"
				case "generation":
					receipt.Generation = ""
				case "actor":
					receipt.Actor = "other"
				case "thread":
					receipt.Thread = "other"
				case "workroot":
					receipt.Workroot = "/other"
				}
				_ = ws.WriteJSON(workerwire.Frame{T: "launched", CmdID: f.CmdID, Session: f.Session, PID: 999999, NativeContext: receipt})
				return true
			}
			r.frameHook.Store(&hook)
			if _, err := r.svc.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, options); err == nil {
				t.Fatal("mismatched readiness accepted")
			}
			child, err := r.db.LatestSessionForAgent(ctx, source.LogicalAgentID)
			if err != nil || child.ID == source.ID || child.State != "launching" {
				t.Fatalf("unsafe child: %+v %v", child, err)
			}
			if receipt, err := r.svc.GetWorkerNativeAdmission(child.ID); err != nil || receipt != nil {
				t.Fatalf("false admission: %+v %v", receipt, err)
			}
		})
	}
}

func TestWorkerCreateRelaunchPersistsCreator(t *testing.T) {
	r, ctx, source, _, _ := nativeContinuationRig(t)
	in := CreateSessionInput{LaunchID: "seat", Worker: "fake-worker", Continuity: "relaunch", IdempotencyPrincipal: "worker-creator", IdempotencyKey: "worker-create"}
	child, err := r.svc.CreateSessionWithInput(in)
	if err != nil {
		t.Fatal(err)
	}
	row, err := r.db.GetSession(child.SessionID)
	if err != nil || row.WorkerID != source.WorkerID || row.Intent != "resume" || row.ParentSessionID.String != source.ID || row.RequestingPrincipal.String != "worker-creator" {
		t.Fatalf("worker create=%+v %v", row, err)
	}
	before, _, err := r.db.ConversationForSession(ctx, child.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := r.svc.CreateSessionWithInput(in)
	if err != nil || !replay.Replayed || replay.SessionID != child.SessionID {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	after, _, err := r.db.ConversationForSession(ctx, child.SessionID)
	if err != nil || after.ID != before.ID || after.Generation != before.Generation {
		t.Fatalf("replay moved conversation: before=%+v after=%+v %v", before, after, err)
	}
}
