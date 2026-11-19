package app

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestReplacementLineageBindsActorsAndSurvivesHistoricalChain(t *testing.T) {
	r, in, actor := replacementStoreRig(t)
	ctx := context.Background()
	if err := r.svc.Store.UpdateSessionState(in.Source.ID, "orphaned", int(in.CheckedPID), nil); err != nil {
		t.Fatal(err)
	}
	row, err := r.svc.Store.GetSession(in.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	in.Source = *row
	if _, err := prepareReplacement(t, r, in); err != nil {
		t.Fatal(err)
	}
	if proof, err := r.svc.Store.GetReplacementLineage(ctx, in.DestinationID); err != nil || proof != nil {
		t.Fatalf("unstarted replacement proof=%+v %v", proof, err)
	}
	if err := r.svc.Store.UpdateSessionState(in.DestinationID, "running", 7, nil); err != nil {
		t.Fatal(err)
	}
	proof, err := r.svc.Store.GetReplacementLineage(ctx, in.DestinationID)
	if err != nil || proof == nil || proof.Kind != "retained_team_v1" || proof.IntentKey != in.IntentKey || proof.SourceActorURI != actor || proof.ReplacementActorURI != actor || proof.ActorURI != actor || proof.SourceSessionID != in.Source.ID || proof.LogicalAgentID != in.Source.LogicalAgentID {
		t.Fatalf("proof=%+v %v", proof, err)
	}
	// Simulate B's admitted credential, without proxy/history calls. A second
	// replacement must retain the credential floor earned by the first.
	floor, ok, err := r.svc.Store.ReplacementCredentialFloor(ctx, in.DestinationID)
	if err != nil || !ok {
		t.Fatalf("credential floor: %v %v", ok, err)
	}
	if _, err := identity.NewStore(r.svc.Store.DB()).Mint(ctx, identity.Principal{ID: "msg://session/local/" + in.DestinationID, Kind: "session", SessionID: in.DestinationID, Scopes: floor.Scopes, ExpiresAt: floor.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	// Build B->C without a proxy call or a read/launch of B's plan by an agent.
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	exit := -1
	if err := r.svc.Store.UpdateSessionState(in.DestinationID, "failed", child.Process.Pid, &exit); err != nil {
		t.Fatal(err)
	}
	source, err := r.svc.Store.GetSession(in.DestinationID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.svc.Store.GetLaunchPlan(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := r.svc.Store.DB().QueryRow(`SELECT plan_json FROM launch_plans WHERE session_id=?`, source.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	plan.ResumeSourceSessionID = source.ID
	plan.RecoveryActorURI = actor
	next := store.TeamReplacementInput{Source: *source, SourcePlanJSON: raw, DestinationID: "third-" + source.ID, Workspace: filepath.Join(t.TempDir(), "third"), IntentKey: in.IntentKey, Plan: plan, CheckedPID: int64(child.Process.Pid), CredentialMode: in.CredentialMode}
	if _, err := prepareReplacement(t, r, next); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.UpdateSessionState(next.DestinationID, "running", 8, nil); err != nil {
		t.Fatal(err)
	}
	historical, err := r.svc.Store.GetReplacementLineage(ctx, source.ID)
	if err != nil || historical == nil || *historical != *proof {
		t.Fatalf("historical edge lost: %+v %v", historical, err)
	}
	tip, err := r.svc.Store.GetReplacementLineage(ctx, next.DestinationID)
	if err != nil || tip == nil || tip.SourceSessionID != source.ID || tip.ActorURI != actor {
		t.Fatalf("tip=%+v %v", tip, err)
	}
	native, err := r.svc.Store.GetWorkerNativeAdmission(next.DestinationID)
	if err != nil || native != nil {
		t.Fatalf("team lineage became native: %+v %v", native, err)
	}
}

func TestReplacementLineageRejectsActorAndAgentMismatch(t *testing.T) {
	for _, name := range []string{"source-actor", "port-actor", "destination-actor", "source-agent", "both-row-agents", "destination-plan-agent", "source-digest", "remote-placement"} {
		t.Run(name, func(t *testing.T) {
			r, in, _ := replacementStoreRig(t)
			if name == "source-actor" {
				if _, err := r.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.recovery_actor_uri','different') WHERE session_id=?`, in.Source.ID); err != nil {
					t.Fatal(err)
				}
				if err := r.svc.Store.DB().QueryRow(`SELECT plan_json FROM launch_plans WHERE session_id=?`, in.Source.ID).Scan(&in.SourcePlanJSON); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareReplacement(t, r, in); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "port-actor":
				if _, err := r.svc.Store.DB().Exec(`UPDATE team_port_intents SET request=json_set(CAST(request AS TEXT),'$.Actor','different') WHERE port_kind='session' AND intent_key=?`, in.IntentKey); err != nil {
					t.Fatal(err)
				}
			case "destination-actor":
				if _, err := r.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.recovery_actor_uri','different') WHERE session_id=?`, in.DestinationID); err != nil {
					t.Fatal(err)
				}
			case "source-agent":
				if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET logical_agent_id='different' WHERE id=?`, in.Source.ID); err != nil {
					t.Fatal(err)
				}
			case "both-row-agents":
				if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET logical_agent_id='different' WHERE id IN (?,?)`, in.Source.ID, in.DestinationID); err != nil {
					t.Fatal(err)
				}
			case "destination-plan-agent":
				if _, err := r.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.logical_agent_id','different') WHERE session_id=?`, in.DestinationID); err != nil {
					t.Fatal(err)
				}
			case "source-digest":
				// Simulate corruption past the immutable-write fence to exercise the
				// independent committed-source digest check at the read boundary.
				if _, err := r.svc.Store.DB().Exec(`DROP TRIGGER replaced_launch_plan_update`); err != nil {
					t.Fatal(err)
				}
				if _, err := r.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.boot_prompt','corrupt') WHERE session_id=?`, in.Source.ID); err != nil {
					t.Fatal(err)
				}
			case "remote-placement":
				if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET worker_id='remote' WHERE id=?`, in.DestinationID); err != nil {
					t.Fatal(err)
				}
			}
			if proof, err := r.svc.Store.GetReplacementLineage(context.Background(), in.DestinationID); err == nil || proof != nil {
				t.Fatalf("mismatch accepted: %+v %v", proof, err)
			}
		})
	}
}

func TestWorkerRelaunchReceiptComesOnlyFromExactCommandVerdict(t *testing.T) {
	for _, name := range []string{"success", "wrong-session", "state-instead-of-launched", "worker-rejected", "unknown-outcome"} {
		t.Run(name, func(t *testing.T) {
			r, ctx, source, _, _ := nativeContinuationRig(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if name != "success" {
				hook := func(ws *websocket.Conn, f workerwire.Frame) bool {
					if f.T != "launch" {
						return false
					}
					reply := workerwire.Frame{T: "launched", CmdID: f.CmdID, Session: f.Session, PID: 7}
					switch name {
					case "wrong-session":
						reply.Session = "different"
					case "state-instead-of-launched":
						reply.T = "state"
						reply.State = "running"
					case "worker-rejected":
						reply.Error = "launch rejected"
					case "unknown-outcome":
						cancel()
						return true
					}
					_ = ws.WriteJSON(reply)
					return true
				}
				r.frameHook.Store(&hook)
			}
			child, err := r.svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "seat", Worker: "fake-worker", Continuity: "relaunch"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.svc.LaunchSessionWithContext(ctx, child.SessionID)
			if name == "success" && err != nil {
				t.Fatal(err)
			}
			if name != "success" {
				expected := "acknowledgement identity unconfirmed"
				if name == "worker-rejected" {
					expected = "launch rejected"
				}
				if name == "unknown-outcome" {
					expected = "context canceled"
				}
				if err == nil || !strings.Contains(err.Error(), expected) {
					t.Fatalf("bad verdict=%v, want %s", err, expected)
				}
			}
			if name == "unknown-outcome" {
				// An eventual generic state report is not an authenticated command verdict.
				if err := r.svc.ingestWorkerFrame(source.WorkerID, workerwire.Frame{T: "state", Session: child.SessionID, State: "running", PID: 7}); err != nil {
					t.Fatal(err)
				}
				ctx = context.Background()
			}
			receipt, readErr := r.db.GetWorkerLineage(ctx, child.SessionID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if name == "success" {
				if receipt == nil || receipt.SourceSessionID != source.ID || receipt.WorkerID != source.WorkerID {
					t.Fatalf("receipt=%+v", receipt)
				}
			} else if receipt != nil {
				t.Fatalf("unconfirmed verdict minted receipt: %+v", receipt)
			}
			native, readErr := r.db.GetWorkerNativeAdmission(child.SessionID)
			if readErr != nil || native != nil {
				t.Fatalf("legacy launch became native: %+v %v", native, readErr)
			}
			raw, _ := json.Marshal(receipt)
			if strings.Contains(string(raw), "native_thread") || strings.Contains(string(raw), "workroot") {
				t.Fatal("UI receipt contains native/private material")
			}
		})
	}
}

func TestOptInClaudeWorkerResumeLineageRequiresExactAck(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "unknown"}[unknown], func(t *testing.T) {
			r, ctx, source, opts, _ := claudeContinuationRig(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			type observation struct {
				id    string
				spec  workerwire.LaunchSpec
				err   error
				proof *store.WorkerLineage
			}
			observed := make(chan observation, 1)
			hook := func(ws *websocket.Conn, f workerwire.Frame) bool {
				if f.T != "launch" {
					return false
				}
				var spec workerwire.LaunchSpec
				err := json.Unmarshal(f.Spec, &spec)
				proof, readErr := r.db.GetWorkerLineage(context.Background(), f.Session)
				if err == nil {
					err = readErr
				}
				observed <- observation{f.Session, spec, err, proof}
				if unknown {
					cancel()
					return true
				}
				_ = ws.WriteJSON(workerwire.Frame{T: "launched", CmdID: f.CmdID, Session: f.Session, PID: 7})
				return true
			}
			r.frameHook.Store(&hook)
			result, err := r.svc.ResumeLogicalAgentWithContext(ctx, source.LogicalAgentID, opts)
			if unknown {
				if err == nil || !strings.Contains(err.Error(), "context canceled") {
					t.Fatalf("unknown result: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			o := <-observed
			if o.err != nil || o.proof != nil || o.spec.ResumeSessionID != "claude-thread" {
				t.Fatalf("before ack %+v", o)
			}
			if !unknown && result.SessionID != o.id {
				t.Fatal("wrong replacement")
			}
			if unknown {
				if err := r.svc.ingestWorkerFrame(source.WorkerID, workerwire.Frame{T: "state", Session: o.id, State: "running", PID: 7}); err != nil {
					t.Fatal(err)
				}
			}
			proof, err := r.db.GetWorkerLineage(context.Background(), o.id)
			if err != nil {
				t.Fatal(err)
			}
			if unknown {
				if proof != nil {
					t.Fatalf("unknown acquired proof %+v", proof)
				}
			} else if proof == nil || proof.SourceSessionID != source.ID || proof.WorkerID != source.WorkerID {
				t.Fatalf("proof %+v", proof)
			}
			native, err := r.db.GetWorkerNativeAdmission(o.id)
			if err != nil || native != nil {
				t.Fatalf("ordinary became native %+v %v", native, err)
			}
		})
	}
}
