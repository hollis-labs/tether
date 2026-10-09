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
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
)

// This explicitly synthetic enrolled source has its own complete two-record
// journal and no provider output/input. It does not claim that a real pending
// turn is settled. Both recorded process identities are owned, reaped children.
func codexReplacementRig(t *testing.T) (*codexRig, store.TeamReplacementInput, shimcodex.State, shimhost.Receipt, string) {
	t.Helper()
	r, in, actor := replacementStoreRig(t)
	db, ctx := r.svc.Store.DB(), context.Background()
	old := in.Source.ID
	id := "accounted-" + old
	plan, err := r.svc.Store.GetLaunchPlan(old)
	if err != nil {
		t.Fatal(err)
	}
	row := in.Source
	row.ID, row.State, row.Workspace = id, "detached", filepath.Join(t.TempDir(), id)
	row.ExitCode, row.EndedAt = sql.NullInt64{}, sql.NullString{}
	if err := r.svc.Store.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	// Fixture setup establishes the original receipt for this source, before
	// invoking production replacement. It is not a replacement implementation.
	for _, query := range []string{
		`UPDATE runtime_bindings SET session_id=? WHERE session_id=?`,
		`UPDATE team_host_intents SET member=json_set(member,'$.session_id',?) WHERE json_extract(member,'$.session_id')=?`,
		`UPDATE team_rosters SET payload=json_set(payload,'$.members[0].session_id',?) WHERE json_extract(payload,'$.members[0].session_id')=?`,
		`UPDATE team_port_intents SET payload=json_quote(?) WHERE port_kind='session' AND CAST(payload AS TEXT)=json_quote(?)`,
		`UPDATE session_idempotency SET session_id=? WHERE session_id=?`,
	} {
		if _, err := db.Exec(query, id, old); err != nil {
			t.Fatal(err)
		}
	}
	var policyJSON string
	if err := db.QueryRow(`SELECT policy_json FROM session_mcp_policy WHERE session_id=?`, old).Scan(&policyJSON); err != nil {
		t.Fatal(err)
	}
	var policy mcpgateway.SessionPolicy
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	policy.SessionID = id
	policy = policy.Seal()
	sealed, _ := json.Marshal(policy)
	if _, err := db.Exec(`INSERT INTO session_mcp_policy(session_id,policy_json) VALUES(?,?)`, id, string(sealed)); err != nil {
		t.Fatal(err)
	}
	r.svc.Catalog.Global.Identity.Mode = "off"
	in.CredentialMode = "off"
	in.Source, err = getReplacementFixtureRow(r, id)
	if err != nil {
		t.Fatal(err)
	}
	in.DestinationID, in.Workspace = "next-"+id, filepath.Join(t.TempDir(), "next")
	if err := db.QueryRow(`SELECT plan_json FROM launch_plans WHERE session_id=?`, id).Scan(&in.SourcePlanJSON); err != nil {
		t.Fatal(err)
	}
	in.Plan.ResumeSourceSessionID = id
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := shim.OpenJournal(filepath.Join(root, "j"), id, 1, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	var state shimcodex.State
	for _, kind := range []string{"shim.launch_intent", "shim.started"} {
		payload, _ := json.Marshal(map[string]string{"instance": "fixture"})
		event, err := journal.Append(mesh.Event{SchemaVersion: "1", ID: kind, Kind: kind, Time: time.Now().UTC(), SessionID: id, Source: mesh.EventSource{Channel: "shim", Confidence: 1}, Actor: mesh.Actor{URN: "msg://service/local/shim", Kind: mesh.ActorService}, Subject: mesh.URN("urn:session:" + id), Generation: 1, ContentType: "application/json", Visibility: "private", Payload: payload}, false)
		if err != nil {
			t.Fatal(err)
		}
		if state.Version == "" {
			jid, _, _ := strings.Cut(event.Cursor, ":")
			state = shimcodex.State{Version: shimcodex.Version, Binding: shimcodex.Binding{Session: id, Instance: "fixture", Operation: "owned-accounted", Attempt: "fixture-attempt", Fingerprint: "fixture-fingerprint", Generation: 1, Journal: jid}, Revision: 1, Epoch: 1, NextID: shimcodex.FirstID}
		}
		raw, _ := json.Marshal(struct {
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}{kind, payload})
		state.Inbox = append(state.Inbox, shimcodex.Event{Identity: state.Binding.Journal + ":" + event.Cursor + ":" + kind, Cursor: event.Cursor, Raw: raw})
		state.Cursor = event.Cursor
	}
	pids := []int{}
	for range 2 {
		child := exec.Command("true")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, child.Process.Pid)
		if err := child.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	receipt := shimhost.Receipt{Session: id, OperationKey: state.Binding.Operation, Instance: state.Binding.Instance, SubmissionAttemptID: state.Binding.Attempt, Fingerprint: state.Binding.Fingerprint, Generation: 1, Backend: "detached", DescriptorPath: filepath.Join(root, "launch.json"), SocketPath: filepath.Join(root, "control.sock"), Journal: state.Binding.Journal, HostPID: pids[0], ShimPID: pids[0], ProviderPID: pids[1], HostStartTime: 123, Attempted: true}
	if err := shimhost.WritePrivateJSON(filepath.Join(root, "placement.json"), receipt); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.persistShim(ctx, id, "codex", "fixture-boot", receipt); err != nil {
		t.Fatal(err)
	}
	p, err := r.svc.Store.CodexProtocolStore(ctx, id, receipt.OperationKey, shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx, 0, state); err != nil {
		t.Fatal(err)
	}
	projection, err := shimcodex.BuildDeliveryProjection(state)
	if err != nil {
		t.Fatal(err)
	}
	next := state
	next.Revision++
	next.Inbox = nil
	projection.ProtocolRevision, projection.DeliveredHighWater = next.Revision, state.Cursor
	next.Delivery = &projection
	if err := p.CommitDelivery(ctx, state, next); err != nil {
		t.Fatal(err)
	}
	receipt.Retired = true
	if err := shimhost.WritePrivateJSON(filepath.Join(root, "placement.json"), receipt); err != nil {
		t.Fatal(err)
	}
	return r, in, next, receipt, actor
}

func getReplacementFixtureRow(r *codexRig, id string) (store.SessionRow, error) {
	row, err := r.svc.Store.GetSession(id)
	if err != nil {
		return store.SessionRow{}, err
	}
	return *row, nil
}

func TestCodexReplacementFreezesHistoricalCustodyAndStartsPristineDestination(t *testing.T) {
	r, in, observed, canonical, actor := codexReplacementRig(t)
	ctx := context.Background()
	oldCustody, err := r.svc.Store.SessionShim(ctx, in.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.svc.Store.CodexProtocolStore(ctx, in.Source.ID, canonical.OperationKey, shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := r.svc.Store.AccountCodexReplacement(ctx, observed, canonical)
	if err != nil {
		t.Fatal(err)
	}
	var originalJSON string
	if err := r.svc.Store.DB().QueryRow(`SELECT state_json FROM codex_shim_protocol WHERE session_id=?`, in.Source.ID).Scan(&originalJSON); err != nil {
		t.Fatal(err)
	}
	ts, err := teamstore.New(r.svc.Store.DB(), teamstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.WithTransaction(ctx, func(conn *sql.Conn) error {
		_, err := r.svc.Store.PrepareCodexTeamReplacementTx(ctx, conn, in, proof, canonical)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil || binding.SessionID != in.DestinationID {
		t.Fatal("original host binding did not move", err)
	}
	currentCustody, err := r.svc.Store.SessionShim(ctx, in.Source.ID)
	if err != nil || currentCustody != oldCustody {
		t.Fatal("old custody was removed/rewritten", err)
	}
	var frozenJSON, evidence string
	if err := r.svc.Store.DB().QueryRow(`SELECT p.state_json,x.obligation_proof_ref FROM codex_shim_protocol p JOIN session_replacements x ON x.source_session_id=p.session_id WHERE p.session_id=?`, in.Source.ID).Scan(&frozenJSON, &evidence); err != nil || frozenJSON != originalJSON || evidence == "" {
		t.Fatal("old ledger/evidence not preserved", err)
	}
	if _, err := p.Load(ctx); err == nil {
		t.Fatal("old operational handle read historical ledger")
	}
	late := observed
	late.Revision++
	if err := p.Commit(ctx, observed.Revision, late); err == nil {
		t.Fatal("old handle committed after replacement")
	}
	if err := p.CommitDelivery(ctx, observed, late); err == nil {
		t.Fatal("old delivery CAS committed after replacement")
	}
	for _, query := range []string{`DELETE FROM session_shims WHERE session_id=?`, `DELETE FROM codex_shim_protocol WHERE session_id=?`} {
		if _, err := r.svc.Store.DB().Exec(query, in.Source.ID); err == nil {
			t.Fatal("old custody/history removal succeeded")
		}
	}
	r.svc.shimHosting = &shimHosting{
		stop: func(context.Context, shimhost.Receipt) error {
			t.Fatal("historical stop reached provider signaling")
			return nil
		},
		inspect: func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
			t.Fatal("historical custody reached operational inspection")
			return shimhost.Inspection{}, nil
		},
	}
	if handled, err := r.svc.stopShimSession(in.Source.ID); !handled || err == nil {
		t.Fatal("old stop was operationally dispatched", err)
	}
	if !r.svc.detachShimSession(ctx, in.Source.ID) || r.svc.reattachCodexShim(ctx, oldCustody, canonical) == nil {
		t.Fatal("historical custody was treated as a live controller")
	}
	if _, draining := r.svc.shimDraining.Load(in.Source.ID); draining {
		t.Fatal("historical detach changed operational bridge state")
	}
	r.svc.settleShimBridgeExit(in.Source.ID)
	if !r.svc.reconcileShimContext(ctx, store.StaleSession{ID: in.Source.ID}) {
		t.Fatal("historical custody entered ordinary orphan revocation")
	}
	if _, err := r.svc.deliverCodexInbox(ctx, observed); err == nil {
		t.Fatal("historical custody admitted active delivery")
	}
	if err := r.svc.currentShimExecution(ctx, in.DestinationID); err != nil {
		t.Fatal("replacement was fenced as its source", err)
	}
	// A distinct new placement initializes through the ordinary protocol store.
	// No old state, inbox, RPC counters or delivery receipt is supplied to it.
	newRoot := t.TempDir()
	newCustody := store.SessionShimRow{SessionID: in.DestinationID, ShimKey: "new-placement", JournalID: "new-journal", Runtime: "codex", RuntimeGeneration: 1, BootGeneration: "new-boot", HostBackend: "detached", DescriptorPath: filepath.Join(newRoot, "launch.json"), SocketPath: filepath.Join(newRoot, "control.sock")}
	if err := r.svc.Store.UpsertSessionShim(ctx, newCustody); err != nil {
		t.Fatal(err)
	}
	newProtocol, err := r.svc.Store.CodexProtocolStore(ctx, in.DestinationID, newCustody.ShimKey, shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	pristine := shimcodex.State{Version: shimcodex.Version, Binding: shimcodex.Binding{Session: in.DestinationID, Operation: newCustody.ShimKey, Instance: "new-instance", Generation: 1}, Revision: 1, NextID: shimcodex.FirstID}
	if err := newProtocol.Commit(ctx, 0, pristine); err != nil {
		t.Fatal(err)
	}
	loaded, err := newProtocol.Load(ctx)
	if err != nil || loaded.Revision != 1 || loaded.NextID != shimcodex.FirstID || loaded.ThreadID != "" || loaded.Delivery != nil || len(loaded.Inbox) != 0 {
		t.Fatal("new protocol inherited old execution data", err)
	}
	if _, err := r.svc.Store.AccountCodexReplacement(ctx, observed, canonical); err != nil {
		t.Fatal("trusted historical reader lost old evidence after freeze", err)
	}
}

func TestHistoricalShimDispatchRefusesLookupFailure(t *testing.T) {
	r, in, _, _, _ := codexReplacementRig(t)
	if err := r.svc.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if r.svc.currentShimExecution(context.Background(), in.Source.ID) == nil {
		t.Fatal("lookup failure admitted operational dispatch")
	}
	if handled, err := r.svc.stopShimSession(in.Source.ID); !handled || err == nil {
		t.Fatal("lookup failure fell through to direct stop", err)
	}
	if !r.svc.detachShimSession(context.Background(), in.Source.ID) {
		t.Fatal("lookup failure fell through to direct detach/kill")
	}
}

func TestCodexReplacementRefusesPendingRevokedChangedAndUncertainEvidence(t *testing.T) {
	for _, name := range []string{"nil proof", "pending inbox", "native commit wins", "live host", "missing provider PID", "changed receipt", "revoked binding", "explicit stop", "rollback"} {
		t.Run(name, func(t *testing.T) {
			r, in, observed, canonical, actor := codexReplacementRig(t)
			ctx := context.Background()
			proof, err := r.svc.Store.AccountCodexReplacement(ctx, observed, canonical)
			if err != nil {
				t.Fatal(err)
			}
			db := r.svc.Store.DB()
			switch name {
			case "nil proof":
				proof = nil
			case "pending inbox":
				if _, err := db.Exec(`UPDATE codex_shim_protocol SET state_json=json_set(state_json,'$.inbox',json('[{"identity":"pending"}]')) WHERE session_id=?`, in.Source.ID); err != nil {
					t.Fatal(err)
				}
			case "native commit wins":
				protocol, err := r.svc.Store.CodexProtocolStore(ctx, in.Source.ID, canonical.OperationKey, shimcodex.ProjectionBudget)
				if err != nil {
					t.Fatal(err)
				}
				next := observed
				next.Revision++
				next.Epoch++
				if err := protocol.Commit(ctx, observed.Revision, next); err != nil {
					t.Fatal("existing controller could not commit before replacement", err)
				}
			case "live host":
				canonical.HostPID = os.Getpid()
			case "missing provider PID":
				canonical.ProviderPID = 0
			case "changed receipt":
				canonical.Fingerprint = "changed"
			case "revoked binding":
				if _, err := db.Exec(`UPDATE runtime_bindings SET revoked_at='revoked' WHERE target_urn=?`, actor); err != nil {
					t.Fatal(err)
				}
			case "explicit stop":
				if _, err := db.Exec(`UPDATE team_port_intents SET ended='stop' WHERE port_kind='session'`); err != nil {
					t.Fatal(err)
				}
			case "rollback":
				if _, err := db.Exec(`CREATE TRIGGER reject_codex_lineage BEFORE INSERT ON session_replacements BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ts, err := teamstore.New(db, teamstore.Options{})
			if err != nil {
				t.Fatal(err)
			}
			err = ts.WithTransaction(ctx, func(conn *sql.Conn) error {
				_, err := r.svc.Store.PrepareCodexTeamReplacementTx(ctx, conn, in, proof, canonical)
				return err
			})
			if err == nil {
				t.Fatal("unproven replacement committed")
			}
			if _, err := r.svc.Store.GetSession(in.DestinationID); !errors.Is(err, store.ErrSessionNotFound) {
				t.Fatal("refusal allocated destination", err)
			}
			binding, err := r.svc.Registry.CurrentBinding(ctx, actor)
			if name != "revoked binding" && (err != nil || binding.SessionID != in.Source.ID) {
				t.Fatal("refusal remapped original binding", err)
			}
			if _, err := r.svc.Store.SessionShim(ctx, in.Source.ID); err != nil {
				t.Fatal("refusal removed old custody", err)
			}
		})
	}
}

func TestAccountedRetiredCodexTeamRecoversThroughOriginalReceipt(t *testing.T) {
	r, in, _, canonical, actor := codexReplacementRig(t)
	ctx := context.Background()
	before, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.RecoverTeamSession(ctx, "retained", in.Source.ID); err != nil {
		t.Fatal(err)
	}
	lineage, err := r.svc.Store.SessionReplacement(ctx, in.Source.ID)
	if err != nil || lineage.ReplacementID == in.Source.ID {
		t.Fatal("retired source was not replaced", err)
	}
	r.wait(2)
	r.idle(lineage.ReplacementID)
	if err := r.svc.RecoverTeamSession(ctx, "retained", lineage.ReplacementID); err != nil {
		t.Fatal("committed destination retry failed", err)
	}
	after, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil || before.ID != after.ID || before.Generation != after.Generation || before.AttemptID != after.AttemptID || after.SessionID != lineage.ReplacementID {
		t.Fatal("recovery changed original host authority", err)
	}
	tracked, err := r.svc.Store.SessionShim(ctx, in.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := loadShimReceipt(tracked)
	if err != nil || actual != canonical {
		t.Fatal("new execution rewrote old custody", err)
	}
}

func TestGoneCodexAccountingRetirementPreservesCustodyForReceiptOwner(t *testing.T) {
	r, in, observed, canonical, actor := codexReplacementRig(t)
	ctx := context.Background()
	canonical.Retired = false
	if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(canonical.DescriptorPath), "placement.json"), canonical); err != nil {
		t.Fatal(err)
	}
	tracked, err := r.svc.Store.SessionShim(ctx, in.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	host := &shimHosting{stop: func(_ context.Context, receipt shimhost.Receipt) error {
		called = true
		if receipt != canonical {
			t.Fatal("retirement did not use exact old custody")
		}
		receipt.Retired = true
		return shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(receipt.DescriptorPath), "placement.json"), receipt)
	}}
	if err := r.svc.retireGoneCodexTeam(ctx, tracked, canonical, host); err != nil || !called {
		t.Fatal("fully accounted gone retirement failed", err)
	}
	if _, err := r.svc.Store.SessionReplacement(ctx, in.Source.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("startup retired custody without original receipt remap lock", err)
	}
	binding, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil || binding.SessionID != in.Source.ID {
		t.Fatal("startup retirement moved enrollment", err)
	}
	current, err := r.svc.Store.SessionShim(ctx, in.Source.ID)
	if err != nil || current != tracked {
		t.Fatal("startup removed historical custody", err)
	}
	canonical.Retired = true
	if _, err := r.svc.Store.AccountCodexReplacement(ctx, observed, canonical); err != nil {
		t.Fatal("retirement erased complete history", err)
	}
}
