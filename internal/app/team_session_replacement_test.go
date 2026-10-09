//go:build !windows

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/messaging/delivery"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/workspace"
)

func replacementStoreRig(t *testing.T) (*codexRig, store.TeamReplacementInput, string) {
	t.Helper()
	r, id, actor := retainedRecoveryRig(t)
	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET pid=? WHERE id=?`, pid, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Store.DB().Exec(`UPDATE team_port_intents SET nonce='retained-nonce' WHERE port_kind='session' AND intent_key='retained'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Store.DB().Exec(`INSERT INTO session_idempotency(key,operation,request_digest,session_id) VALUES('team-port:retained-nonce','create','original-request',?)`, id); err != nil {
		t.Fatal(err)
	}
	row, err := r.svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.svc.Store.GetLaunchPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := r.svc.Store.DB().QueryRow(`SELECT plan_json FROM launch_plans WHERE session_id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	plan.ResumeSourceSessionID = id
	plan.RecoveryActorURI = actor
	plan.RecoveryPrompt = "continue existing assigned work"
	return r, store.TeamReplacementInput{Source: *row, SourcePlanJSON: raw, DestinationID: "next-" + id, Workspace: filepath.Join(t.TempDir(), "next"), IntentKey: "retained", Plan: plan, CheckedPID: int64(pid), CredentialMode: r.svc.Catalog.Global.Identity.EffectiveMode()}, actor
}

func prepareReplacement(t *testing.T, r *codexRig, in store.TeamReplacementInput) (store.SessionReplacement, error) {
	t.Helper()
	ts, err := teamstore.New(r.svc.Store.DB(), teamstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var result store.SessionReplacement
	err = ts.WithTransaction(context.Background(), func(conn *sql.Conn) error {
		var err error
		result, err = r.svc.Store.PrepareTeamReplacementTx(context.Background(), conn, in)
		return err
	})
	return result, err
}

func TestTeamReplacementAtomicallyRemapsOriginalAuthorityAndFreezesOldExecution(t *testing.T) {
	r, in, actor := replacementStoreRig(t)
	ctx := context.Background()
	original, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.svc.Store.DB().Exec(`INSERT INTO team_roster_snapshots(run_id,version,payload) SELECT run_id,version,payload FROM team_rosters WHERE run_id='run'`); err != nil {
		t.Fatal(err)
	}
	result, err := prepareReplacement(t, r, in)
	if err != nil || result.ReplacementID != in.DestinationID {
		t.Fatalf("replacement=%+v %v", result, err)
	}
	binding, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	if binding.SessionID != in.DestinationID || binding.ID != original.ID || binding.AttemptID != original.AttemptID || binding.Generation != original.Generation || binding.HostID != original.HostID || binding.LeaseExpiresAt != original.LeaseExpiresAt || binding.TargetURN != original.TargetURN {
		t.Fatal("replacement changed retained binding authority")
	}
	for name, query := range map[string]string{
		"host member":         `SELECT json_extract(member,'$.session_id') FROM team_host_intents WHERE intent_key='retained'`,
		"current roster":      `SELECT json_extract(payload,'$.members[0].session_id') FROM team_rosters WHERE run_id='run'`,
		"session port":        `SELECT json_extract(payload,'$') FROM team_port_intents WHERE port_kind='session' AND intent_key='retained'`,
		"cleanup idempotency": `SELECT session_id FROM session_idempotency WHERE key='team-port:retained-nonce'`,
	} {
		var id string
		if err := r.svc.Store.DB().QueryRow(query).Scan(&id); err != nil || id != in.DestinationID {
			t.Fatalf("%s not remapped: %s %v", name, id, err)
		}
	}
	var historyID string
	if err := r.svc.Store.DB().QueryRow(`SELECT json_extract(payload,'$.members[0].session_id') FROM team_roster_snapshots WHERE run_id='run' AND version=1`).Scan(&historyID); err != nil || historyID != in.Source.ID {
		t.Fatal("historical roster was rewritten", err)
	}
	newRow, err := r.svc.Store.GetSession(in.DestinationID)
	if err != nil || newRow.State != "created" || newRow.ParentSessionID.String != in.Source.ID || newRow.LogicalAgentID != in.Source.LogicalAgentID || newRow.ProviderID != in.Source.ProviderID {
		t.Fatal("replacement identity/lineage changed", err)
	}
	oldRow, err := r.svc.Store.GetSession(in.Source.ID)
	if err != nil || *oldRow != in.Source {
		t.Fatal("historical source row rewritten", err)
	}
	for _, query := range []string{
		`UPDATE sessions SET state='created' WHERE id=?`,
		`UPDATE launch_plans SET plan_json='{}' WHERE session_id=?`,
		`UPDATE session_provider_mappings SET native_session_id='late-id' WHERE session_id=?`,
		`DELETE FROM sessions WHERE id=?`,
	} {
		if _, err := r.svc.Store.DB().Exec(query, in.Source.ID); err == nil {
			t.Fatalf("old execution fence bypassed: %s", query)
		}
	}
	var lineage map[string]string
	var payload string
	if err := r.svc.Store.DB().QueryRow(`SELECT payload_json FROM events WHERE session_id=? AND kind='session.replaced_by'`, in.Source.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(payload), &lineage) != nil || lineage["replaced_by"] != in.DestinationID || lineage["replaces"] != in.Source.ID {
		t.Fatal("durable lineage missing")
	}
	// A retry cannot choose another execution or create duplicate membership.
	if again, err := prepareReplacement(t, r, in); err != nil || again != result {
		t.Fatalf("idempotent retry=%+v %v", again, err)
	}
	in.DestinationID = "different-destination"
	if _, err := prepareReplacement(t, r, in); !errors.Is(err, store.ErrSessionReplacementUnavailable) {
		t.Fatal("second replacement admitted", err)
	}
}

func TestTeamReplacementPreservesPendingRecoveryChannelContextWithoutAdvancingIt(t *testing.T) {
	r, input, actor := replacementStoreRig(t)
	input.Plan.RecoveryCursors = map[string]int64{"recorded-team-channel": 7}
	if _, err := prepareReplacement(t, r, input); err != nil {
		t.Fatal("recovery-only channel context refused", err)
	}
	plan, err := r.svc.Store.GetLaunchPlan(input.DestinationID)
	if err != nil || plan.RecoveryCursors["recorded-team-channel"] != 7 {
		t.Fatal("replacement lost included channel context", err)
	}
	sequence, err := r.svc.Store.RecoveryChannelCursor(context.Background(), actor, "recorded-team-channel")
	if err != nil || sequence != 0 {
		t.Fatal("preparation acknowledged unsubmitted recovery context", err)
	}
}

func TestTeamReplacementRefusesChangedStoppedOrUnaccountedSource(t *testing.T) {
	for _, query := range []string{
		`UPDATE runtime_bindings SET revoked_at='revoked' WHERE host_id='team'`,
		`UPDATE team_port_intents SET ended='stop' WHERE port_kind='session'`,
		`UPDATE team_host_intents SET tombstone='retire'`,
		`UPDATE team_runs SET payload='{"status":"completed"}'`,
		`UPDATE registry_entries SET props_json='{}'`,
		`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.model','changed')`,
		`UPDATE session_idempotency SET session_id='foreign'`,
		`UPDATE team_port_intents SET nonce='' WHERE port_kind='session'`,
	} {
		t.Run(query, func(t *testing.T) {
			r, in, _ := replacementStoreRig(t)
			if _, err := r.svc.Store.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareReplacement(t, r, in); !errors.Is(err, store.ErrSessionReplacementUnavailable) {
				t.Fatalf("changed source admitted: %v", err)
			}
			if _, err := r.svc.Store.GetSession(in.DestinationID); !errors.Is(err, store.ErrSessionNotFound) {
				t.Fatal("refusal allocated replacement", err)
			}
		})
	}
}

func TestTeamReplacementRollsBackAllReferencesOnLateFailure(t *testing.T) {
	r, in, actor := replacementStoreRig(t)
	if _, err := r.svc.Store.DB().Exec(`CREATE TRIGGER fixture_replacement_failure BEFORE INSERT ON session_replacements BEGIN SELECT RAISE(ABORT,'fixture late failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareReplacement(t, r, in); err == nil {
		t.Fatal("late failure ignored")
	}
	binding, err := r.svc.Registry.CurrentBinding(context.Background(), actor)
	if err != nil || binding.SessionID != in.Source.ID {
		t.Fatal("binding partially moved", err)
	}
	keyed, err := r.svc.Store.GetSessionIdempotency("team-port:retained-nonce")
	if err != nil || keyed.SessionID != in.Source.ID {
		t.Fatal("cleanup target partially moved", err)
	}
	if _, err := r.svc.Store.GetSession(in.DestinationID); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatal("failed transaction persisted a destination", err)
	}
	if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET state='orphaned' WHERE id=?`, in.Source.ID); err != nil {
		t.Fatal("rolled-back freeze blocked original source", err)
	}
}

func TestReplacementCredentialKeepsInheritedScopesExpiryAndCurrentRestrictions(t *testing.T) {
	for _, current := range []string{"boot", "narrower_caller"} {
		t.Run(current, func(t *testing.T) {
			r, in, _ := replacementStoreRig(t)
			expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
			if _, err := r.svc.Store.DB().Exec(`UPDATE principals SET scopes_json='["session.write"]',expires_at=? WHERE session_id=?`, expires, in.Source.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareReplacement(t, r, in); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if current == "narrower_caller" {
				ctx = identity.WithPrincipal(ctx, identity.Principal{ID: "fixture-verified-caller", Scopes: []string{"message.write"}})
			}
			if _, err := r.svc.mintSessionCredential(ctx, in.DestinationID); err != nil {
				t.Fatal(err)
			}
			var scopes, at string
			if err := r.svc.Store.DB().QueryRow(`SELECT scopes_json,expires_at FROM principals WHERE session_id=? AND revoked_at IS NULL`, in.DestinationID).Scan(&scopes, &at); err != nil {
				t.Fatal(err)
			}
			var parsed []string
			if json.Unmarshal([]byte(scopes), &parsed) != nil {
				t.Fatal("invalid credential scope ceiling")
			}
			if current == "boot" && (len(parsed) != 1 || parsed[0] != "session.write") || current == "narrower_caller" && len(parsed) != 0 || at != expires {
				t.Fatalf("credential widened original delegation: scopes=%s expires=%s", scopes, at)
			}
			oldPolicy, err := r.svc.Store.RecoverySessionMCPPolicy(ctx, in.Source.ID)
			if err == nil {
				t.Fatal("revoked old execution credential regained read authority", oldPolicy.SessionID)
			}
			policy, err := r.svc.Store.SessionMCPPolicy(ctx, in.DestinationID)
			if err != nil {
				t.Fatal(err)
			}
			policy.Servers = append(policy.Servers, "unearned-server")
			if err := r.svc.Store.SaveSessionMCPPolicy(ctx, policy.Seal()); err == nil {
				t.Fatal("replacement silently widened sealed MCP floor")
			}
		})
	}
}

func TestReplacementCredentialRefusesExpiredOrLostAuthority(t *testing.T) {
	for _, phase := range []string{"expired_before_commit", "missing_before_commit", "revoked_after_commit", "expired_after_commit"} {
		t.Run(phase, func(t *testing.T) {
			r, in, _ := replacementStoreRig(t)
			switch phase {
			case "expired_before_commit":
				if _, err := r.svc.Store.DB().Exec(`UPDATE principals SET expires_at='2000-01-01T00:00:00Z' WHERE session_id=?`, in.Source.ID); err != nil {
					t.Fatal(err)
				}
			case "missing_before_commit":
				if _, err := r.svc.Store.DB().Exec(`DELETE FROM principals WHERE session_id=?`, in.Source.ID); err != nil {
					t.Fatal(err)
				}
			case "expired_after_commit":
				if _, err := r.svc.Store.DB().Exec(`UPDATE principals SET expires_at=? WHERE session_id=?`, time.Now().UTC().Add(2*time.Second).Format(time.RFC3339Nano), in.Source.ID); err != nil {
					t.Fatal(err)
				}
			}
			_, err := prepareReplacement(t, r, in)
			if phase == "expired_before_commit" || phase == "missing_before_commit" {
				if !errors.Is(err, store.ErrSessionReplacementUnavailable) {
					t.Fatal("invalid original credential ceiling admitted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if phase == "revoked_after_commit" {
				if _, err := r.svc.Store.DB().Exec(`UPDATE runtime_bindings SET revoked_at='revoked' WHERE host_id='team'`); err != nil {
					t.Fatal(err)
				}
			} else {
				<-time.After(2100 * time.Millisecond)
			}
			if _, err := r.svc.mintSessionCredential(context.Background(), in.DestinationID); err == nil {
				t.Fatal("invalid inherited authority defaulted to worker credential")
			}
		})
	}
}

func TestReplacementCredentialPreservesIdentityOffAndRefusesModeChange(t *testing.T) {
	r, in, _ := replacementStoreRig(t)
	in.CredentialMode = "off"
	r.svc.Catalog.Global.Identity.Mode = "off"
	if _, err := r.svc.Store.DB().Exec(`DELETE FROM principals WHERE session_id=?`, in.Source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareReplacement(t, r, in); err != nil {
		t.Fatal(err)
	}
	if token, err := r.svc.mintSessionCredential(context.Background(), in.DestinationID); err != nil || token != "" {
		t.Fatal("identity off minted an execution credential", err)
	}
	r.svc.Catalog.Global.Identity.Mode = "observe"
	if _, err := r.svc.mintSessionCredential(context.Background(), in.DestinationID); !errors.Is(err, store.ErrSessionReplacementUnavailable) {
		t.Fatal("mode change silently defaulted to worker authority", err)
	}
	if _, err := identity.NewStore(r.svc.Store.DB()).MintSessionForLaunch(context.Background(), identity.Principal{ID: "fixture", Kind: "session", SessionID: in.DestinationID, Scopes: []string{}}); err == nil {
		t.Fatal("direct credential insert bypassed identity-off ceiling")
	}
}

func TestTeamReplacementRefusesUnknownOrLivingDirectProcess(t *testing.T) {
	for _, pid := range []int{0, os.Getpid()} {
		t.Run(strconv.Itoa(pid), func(t *testing.T) {
			r, in, actor := replacementStoreRig(t)
			if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET pid=? WHERE id=?`, pid, in.Source.ID); err != nil {
				t.Fatal(err)
			}
			if err := r.svc.RecoverTeamSession(context.Background(), in.IntentKey, in.Source.ID); !errors.Is(err, store.ErrSessionReplacementUnavailable) {
				t.Fatal("unknown/live process admitted", err)
			}
			if _, err := r.svc.Store.SessionReplacement(context.Background(), in.Source.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("refusal committed replacement", err)
			}
			binding, err := r.svc.Registry.CurrentBinding(context.Background(), actor)
			if err != nil || binding.SessionID != in.Source.ID || r.count() != 1 {
				t.Fatal("refusal changed retained execution", err)
			}
		})
	}
}

func TestTeamReplacementFencesDeliveryCommitOrdersAndPreservesAcceptedReceipt(t *testing.T) {
	for _, order := range []string{"claim_first", "replace_first", "accepted_history"} {
		t.Run(order, func(t *testing.T) {
			r, in, actor := replacementStoreRig(t)
			ctx := context.Background()
			from, err := messaging.ParseURN("msg://user/local/fixture")
			if err != nil {
				t.Fatal(err)
			}
			to, err := messaging.ParseURN(actor)
			if err != nil {
				t.Fatal(err)
			}
			ds := r.svc.Store.DeliveryStore()
			message, err := ds.Enqueue(ctx, delivery.EnqueueRequest{From: from, Recipients: []delivery.RecipientTarget{{Address: to}}, Kind: messaging.MsgKindNotice, Payload: []byte("retained obligation")})
			if err != nil {
				t.Fatal(err)
			}
			request := delivery.ClaimRequest{DeliveryID: message.Deliveries[0].ID, Holder: in.Source.ID, LeaseDuration: time.Minute, BindingGeneration: 1}
			if order != "replace_first" {
				claimed, err := ds.Claim(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				lease := delivery.LeaseRef{DeliveryID: claimed.Attempt.DeliveryID, AttemptID: claimed.Attempt.ID, LeaseToken: claimed.Attempt.LeaseToken, BindingGeneration: claimed.Attempt.BindingGeneration}
				if order == "claim_first" {
					if _, err := prepareReplacement(t, r, in); !errors.Is(err, store.ErrSessionReplacementUnavailable) {
						t.Fatal("replacement inherited unsettled old claim", err)
					}
					if _, _, err := ds.Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: true}); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageConsumed}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := prepareReplacement(t, r, in); err != nil {
					t.Fatal(err)
				}
				if order == "accepted_history" {
					if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageConsumed}); err != nil {
						t.Fatal("historical idempotent receipt refused", err)
					}
					return
				}
				// Acknowledging an already failed attempt is library idempotency,
				// not permission to create another receipt or consume the delivery.
				if settled, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageConsumed}); err != nil || settled.Status == delivery.DeliveryDelivered {
					t.Fatal("stale ack changed pending obligation", err)
				}
			} else {
				prepared, starting := make(chan struct{}), make(chan struct{})
				result := make(chan error, 1)
				ts, err := teamstore.New(r.svc.Store.DB(), teamstore.Options{})
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					result <- ts.WithTransaction(ctx, func(conn *sql.Conn) error {
						_, err := r.svc.Store.PrepareTeamReplacementTx(ctx, conn, in)
						close(prepared)
						<-starting
						return err
					})
				}()
				<-prepared
				close(starting)
				if _, err := ds.Claim(ctx, request); err == nil {
					t.Fatal("old claim racing committed remap admitted")
				}
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ds.Claim(ctx, request); err == nil {
				t.Fatal("replaced session acquired a new lease")
			}
			if order == "replace_first" {
				var sequence int
				var name, path string
				if err := r.svc.Store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
					t.Fatal(err)
				}
				reopened, err := store.Open(path)
				if err != nil {
					t.Fatal("reopen required delivery fences", err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				ds = reopened.DeliveryStore()
				if _, err := ds.Claim(ctx, request); err == nil {
					t.Fatal("reopened store admitted old holder")
				}
			}
			request.Holder = in.DestinationID
			if _, err := ds.Claim(ctx, request); err != nil {
				t.Fatal("replacement lost pending original obligation", err)
			}
		})
	}
}

func TestTeamReplacementColdFallbackPreservesHistoricalNativeMapping(t *testing.T) {
	r, in, _ := replacementStoreRig(t)
	old, err := r.svc.Store.GetSessionProviderMapping(in.Source.ID, "tether", in.Plan.ProviderID)
	if err != nil || !old.NativeSessionID.Valid {
		t.Fatal("fixture native identity missing", err)
	}
	in.Plan.ResumeProviderSessionID = old.NativeSessionID.String
	if _, err := prepareReplacement(t, r, in); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.ClearNativeResume(context.Background(), in.Source.ID, in.DestinationID, in.Plan.ProviderID, old.NativeSessionID.String); err != nil {
		t.Fatal(err)
	}
	after, err := r.svc.Store.GetSessionProviderMapping(in.Source.ID, "tether", in.Plan.ProviderID)
	if err != nil || after != old {
		t.Fatal("cold attempt rewrote frozen historical identity", err)
	}
	next, err := r.svc.Store.GetSessionProviderMapping(in.DestinationID, "tether", in.Plan.ProviderID)
	if err != nil || next.NativeSessionID.Valid {
		t.Fatal("cold fallback retained failed replacement native hint", err)
	}
	plan, err := r.svc.Store.GetLaunchPlan(in.DestinationID)
	if err != nil || plan.ResumeProviderSessionID != "" {
		t.Fatal("cold fallback plan can retry failed hint", err)
	}
}

func TestTeamReplacementRetriesCommittedDestinationAndKeepsFailedAdmission(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "crash_before_placement", true: "admission_failed"}[failure], func(t *testing.T) {
			r, in, actor := replacementStoreRig(t)
			ws, err := workspace.Create(filepath.Dir(in.Workspace), in.DestinationID, in.Plan)
			if err != nil {
				t.Fatal(err)
			}
			in.Workspace = ws.Root
			if _, err = prepareReplacement(t, r, in); err != nil {
				t.Fatal(err)
			}
			if failure {
				r.svc.factories[in.Plan.ProviderID] = func(*launch.Plan) (agentsessions.Runtime, error) { return nil, errors.New("fixture admission refused") }
			}
			ctx := context.Background()
			first := r.svc.RecoverTeamSession(ctx, in.IntentKey, in.DestinationID)
			if failure {
				if first == nil {
					t.Fatal("failed admission reported success")
				}
			} else {
				if first != nil {
					t.Fatal(first)
				}
				r.wait(2)
				r.idle(in.DestinationID)
			}
			second := r.svc.RecoverTeamSession(ctx, in.IntentKey, in.DestinationID)
			if failure && !errors.Is(second, store.ErrSessionReplacementUnavailable) || !failure && second != nil {
				t.Fatal("retry did not preserve current destination/admission", second)
			}
			if _, err := r.svc.Store.SessionReplacement(ctx, in.DestinationID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("retry allocated another replacement", err)
			}
			row, err := r.svc.Store.GetSession(in.DestinationID)
			if err != nil || failure && row.State != "failed" || !failure && row.State != "running" {
				t.Fatal("admission state was silently reset", err)
			}
			binding, err := r.svc.Registry.CurrentBinding(ctx, actor)
			if err != nil || binding.SessionID != in.DestinationID {
				t.Fatal("failed admission rolled retained binding back", err)
			}
		})
	}
}
