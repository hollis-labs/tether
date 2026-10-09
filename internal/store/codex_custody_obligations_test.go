//go:build !windows

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/shimcodex"
)

func writeCodexObligationFixture(t *testing.T, db *Store, state shimcodex.State) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.db.Exec(`UPDATE codex_shim_protocol SET revision=?,state_json=? WHERE session_id=?`, state.Revision, string(raw), state.Binding.Session); err != nil {
		t.Fatal(err)
	}
}

func TestCodexCustodyObligationsRetainPrivateWorkWithoutLeaking(t *testing.T) {
	for _, kind := range []string{"intent", "attempted", "written", "unknown", "partial", "inbox", "active", "callback"} {
		t.Run(kind, func(t *testing.T) {
			db, state := codexDeliverySQLFixture(t)
			state.Inbox = nil
			state.ReplayHighWater = state.Cursor
			switch kind {
			case "intent", "attempted", "written", "unknown":
				op := shimcodex.Operation{ID: shimcodex.FirstID, Method: "turn/start", Params: []byte(`{"secret":"PRIVATE-PAYLOAD"}`), Phase: shimcodex.Intent}
				if kind == "attempted" {
					op.Phase = shimcodex.Attempted
				}
				if kind == "written" {
					op.Phase = shimcodex.Written
				}
				if kind == "unknown" {
					op.Phase, op.EffectUnknown = shimcodex.Answered, true
				}
				state.Operations, state.NextID = []shimcodex.Operation{op}, shimcodex.FirstID+1
			case "partial":
				state.Partial = []byte("PRIVATE-PARTIAL")
				state.StreamOffset = uint64(len(state.Partial))
			case "inbox":
				state.Inbox = []shimcodex.Event{{Identity: "secret-event", Cursor: state.Cursor, Raw: []byte(`{"secret":"PRIVATE-INBOX"}`)}}
			case "active":
				state.InitializeID, state.Initialized, state.NextID = shimcodex.FirstID, true, shimcodex.FirstID+1
				state.ThreadID, state.ActiveTurn = "PRIVATE-THREAD", "PRIVATE-TURN"
			case "callback":
				state.ServerRequests = []shimcodex.ServerRequest{{Source: "PRIVATE-SOURCE", ID: []byte(`1`), Method: "approval", Params: []byte(`{"secret":"PRIVATE-CALLBACK"}`)}}
			}
			writeCodexObligationFixture(t, db, state)
			before, err := db.SessionShim(context.Background(), "s")
			if err != nil {
				t.Fatal(err)
			}
			got, err := db.EvaluateCodexCustodyObligations(context.Background(), state)
			if err != nil || !got.SnapshotVerified || !got.Unknown || got.DeliveryVerified || got.Revision != state.Revision {
				t.Fatalf("private work lost: %+v %v", got, err)
			}
			if (kind == "intent" || kind == "attempted" || kind == "written" || kind == "callback") && !got.Pending || kind == "partial" && !got.Partial || kind == "inbox" && !got.Inbox || kind == "active" && !got.Active {
				t.Fatalf("obligation omitted: %+v", got)
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw), "PRIVATE") || got.Refusal == "" {
				t.Fatalf("unsafe diagnostic: %s", raw)
			}
			p, err := db.CodexProtocolStore(context.Background(), "s", "p", shimcodex.ProjectionBudget)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := p.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(state)
			b, _ := json.Marshal(loaded)
			after, err := db.SessionShim(context.Background(), "s")
			if err != nil || string(a) != string(b) || before != after {
				t.Fatal("diagnostic changed protocol or custody")
			}
		})
	}
}

func TestCodexCustodyObligationsEmptyInboxIsNotDelivery(t *testing.T) {
	db, state := codexDeliverySQLFixture(t)
	state.Inbox, state.ReplayHighWater = nil, state.Cursor
	writeCodexObligationFixture(t, db, state)
	db.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := db.EvaluateCodexCustodyObligations(ctx, state)
	if err != nil || !got.SnapshotVerified || got.Inbox || !got.Unknown || got.DeliveryVerified || got.Refusal != CodexCustodyDeliveryUnverified {
		t.Fatalf("empty inbox became proof: %+v %v", got, err)
	}
}

func codexObligationDeliveredFixture(t *testing.T) (*Store, shimcodex.State) {
	t.Helper()
	db, state := codexDeliverySQLFixture(t)
	state.Initialized, state.InitializeID, state.NextID, state.ThreadID = true, shimcodex.FirstID, shimcodex.FirstID+1, "thread"
	writeCodexObligationFixture(t, db, state)
	p, err := db.CodexProtocolStore(context.Background(), "s", "p", shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	e, err := shimcodex.Open(context.Background(), p, state.Binding, state.Epoch, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: shimcodex.ProjectionBudget})
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Join([]string{
		`{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn","status":"inProgress"}}}`,
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"id":"answer","type":"agentMessage","text":""}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","itemId":"answer","delta":"PRIVATE-ANSWER"}}`,
		`{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"id":"answer","type":"agentMessage","text":"PRIVATE-ANSWER"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`,
	}, "\n") + "\n"
	ctx := context.Background()
	if _, err = e.AcceptOutput(ctx, 1, "j:2", "stdout", []byte(data)); err != nil {
		t.Fatal(err)
	}
	if err = e.AcceptReplayHighWater(ctx, 1, "j:2"); err != nil {
		t.Fatal(err)
	}
	state = e.Snapshot()
	projection, err := shimcodex.BuildDeliveryProjection(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Turns) != 1 {
		t.Fatal("fixture lost turn")
	}
	turn := &projection.Turns[0]
	text := shimcodex.ProjectedTurnText(*turn)
	turn.OutputAcceptanceID = TurnOutputID("s", turn.StableOutputTurnID, "final", turn.CompletionSourceID, text)
	output := events.TurnOutputEvent{OutputID: turn.OutputAcceptanceID, SessionID: "s", TurnID: turn.StableOutputTurnID, Kind: "final", ProviderResultID: turn.CompletionSourceID, StopReason: turn.StopReason, Runtime: "codex", Text: text}
	raw, _ := json.Marshal(output)
	if _, _, err = db.InsertEvent(events.ScopeSession, "s", events.KindSessionTurnOutput, string(raw)); err != nil {
		t.Fatal(err)
	}
	projection.ProtocolRevision, projection.DeliveredHighWater = state.Revision+1, state.Cursor
	next := state
	next.Revision, next.Inbox, next.Delivery = state.Revision+1, nil, &projection
	if err = p.CommitDelivery(ctx, state, next); err != nil {
		t.Fatal(err)
	}
	return db, next
}

func TestCodexCustodyObligationsReuseRealPublicReceipt(t *testing.T) {
	db, state := codexObligationDeliveredFixture(t)
	db.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := db.EvaluateCodexCustodyObligations(ctx, state)
	if err != nil || !got.SnapshotVerified || !got.DeliveryVerified || got.Unknown || got.Refusal != "" || got.Active || got.Pending {
		t.Fatalf("real public delivery unavailable: %+v %v", got, err)
	}
	// The public event is part of the proof, even though the inbox remains empty.
	if _, err = db.db.Exec(`UPDATE events SET payload_json=json_set(payload_json,'$.text','TAMPERED') WHERE kind=?`, events.KindSessionTurnOutput); err != nil {
		t.Fatal(err)
	}
	got, err = db.EvaluateCodexCustodyObligations(ctx, state)
	if err != nil || got.DeliveryVerified || !got.Unknown || got.Refusal != CodexCustodyDeliveryUnverified {
		t.Fatalf("tampered receipt accepted: %+v %v", got, err)
	}
}

func TestCodexCustodyObligationsPublicReceiptDoesNotSettleOpenTurn(t *testing.T) {
	db, state := codexObligationDeliveredFixture(t)
	ctx := context.Background()
	p, err := db.CodexProtocolStore(ctx, "s", "p", shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	e, err := shimcodex.Open(ctx, p, state.Binding, state.Epoch, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: shimcodex.ProjectionBudget})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcceptOutput(ctx, 1, "j:3", "stdout", []byte("{\"method\":\"turn/started\",\"params\":{\"threadId\":\"thread\",\"turn\":{\"id\":\"unfinished\",\"status\":\"inProgress\"}}}\n")); err != nil {
		t.Fatal(err)
	}
	if err = e.AcceptReplayHighWater(ctx, 1, "j:3"); err != nil {
		t.Fatal(err)
	}
	if err = e.DeliverInbox(ctx, func(_ context.Context, observed shimcodex.State) (shimcodex.Projection, error) {
		return shimcodex.BuildDeliveryProjection(observed)
	}); err != nil {
		t.Fatal(err)
	}
	state = e.Snapshot()
	got, err := db.EvaluateCodexCustodyObligations(ctx, state)
	if err != nil || !got.SnapshotVerified || !got.DeliveryVerified || !got.Active || got.Refusal != CodexCustodyObligationsPending {
		t.Fatalf("public delivery settled active protocol work: %+v %v", got, err)
	}
}

func TestCodexCustodyObligationsEndedAndUnsupportedReceiptRemainUnavailable(t *testing.T) {
	for _, kind := range []string{"ended", "unsupported_source", "outstanding_input"} {
		t.Run(kind, func(t *testing.T) {
			db, state := codexObligationDeliveredFixture(t)
			switch kind {
			case "ended":
				if _, err := db.db.Exec(`UPDATE sessions SET state='orphaned'`); err != nil {
					t.Fatal(err)
				}
			case "unsupported_source":
				state.Delivery.Sources[0].Disposition = "retained_unsupported"
				writeCodexObligationFixture(t, db, state)
			case "outstanding_input":
				state.Delivery.OutstandingInputIDs = []string{"4294967296"}
				writeCodexObligationFixture(t, db, state)
			}
			got, err := db.EvaluateCodexCustodyObligations(context.Background(), state)
			if err != nil || !got.SnapshotVerified || got.Refusal == "" || kind != "outstanding_input" && (!got.Unknown || got.DeliveryVerified) || kind == "outstanding_input" && !got.Pending {
				t.Fatalf("unsupported obligations omitted: %+v %v", got, err)
			}
		})
	}
}

func TestCodexCustodyObligationsRefuseMissingAmbiguousAndForeignEvidence(t *testing.T) {
	for _, kind := range []string{"missing", "custody_missing", "foreign_key", "generation", "journal", "runtime", "revision", "same_revision_content", "version", "malformed", "unknown_fields", "duplicate_fields", "oversized", "phase", "binding", "epoch", "retired_source"} {
		t.Run(kind, func(t *testing.T) {
			db, state := codexDeliverySQLFixture(t)
			var query string
			switch kind {
			case "missing":
				query = `DELETE FROM codex_shim_protocol`
			case "custody_missing":
				query = `DELETE FROM session_shims`
			case "foreign_key":
				query = `UPDATE codex_shim_protocol SET shim_key='foreign'`
			case "generation":
				query = `UPDATE session_shims SET runtime_generation='2'`
			case "journal":
				query = `UPDATE session_shims SET journal_id='other'`
			case "runtime":
				query = `UPDATE session_shims SET runtime='claude'`
			case "revision":
				state.Revision++
			case "same_revision_content":
				state.Inbox = nil
			case "version":
				query = `UPDATE codex_shim_protocol SET protocol='future'`
			case "malformed":
				query = `UPDATE codex_shim_protocol SET state_json='{"PRIVATE":"broken'`
			case "unknown_fields":
				query = `UPDATE codex_shim_protocol SET state_json=json_set(state_json,'$.unknown','PRIVATE')`
			case "duplicate_fields":
				query = `UPDATE codex_shim_protocol SET state_json='{"version":"future",'||substr(state_json,2)`
			case "oversized":
				if _, err := db.db.Exec(`UPDATE codex_shim_protocol SET state_json=?`, strings.Repeat(" ", shimcodex.ProjectionBudget+1)); err != nil {
					t.Fatal(err)
				}
			case "phase":
				state.Operations = []shimcodex.Operation{{ID: shimcodex.FirstID, Method: "turn/start", Params: []byte(`{}`), Phase: "future"}}
				state.NextID++
				writeCodexObligationFixture(t, db, state)
			case "binding":
				state.Binding.Attempt = ""
				writeCodexObligationFixture(t, db, state)
			case "epoch":
				state.Epoch = 0
				writeCodexObligationFixture(t, db, state)
			case "retired_source":
				query = `UPDATE sessions SET state='orphaned'`
			}
			if query != "" {
				if _, err := db.db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			got, err := db.EvaluateCodexCustodyObligations(context.Background(), state)
			if err != nil || !got.Unknown || got.DeliveryVerified || got.Refusal == "" {
				t.Fatalf("ambiguous evidence accepted: %+v %v", got, err)
			}
			if kind != "retired_source" && got.SnapshotVerified {
				t.Fatalf("invalid snapshot verified: %+v", got)
			}
		})
	}
}

func TestCodexCustodyObligationsCancellationAndUnavailableStore(t *testing.T) {
	db, state := codexDeliverySQLFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := db.EvaluateCodexCustodyObligations(ctx, state)
	if !errors.Is(err, context.Canceled) || !got.Unknown || got.SnapshotVerified {
		t.Fatalf("cancellation ignored: %+v %v", got, err)
	}
	for _, db := range []*Store{nil, {}} {
		got, err := db.EvaluateCodexCustodyObligations(context.Background(), state)
		if err != nil || !got.Unknown || got.Refusal != CodexCustodyUnavailable {
			t.Fatalf("unbacked store accepted: %+v %v", got, err)
		}
	}
}
