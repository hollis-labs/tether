//go:build !windows

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/messaging/delivery"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/shimcodex"
)

// This fixture earns public delivery through the existing production issuer.
// Neither this fixture nor its terminal delivery receipt grants historical
// custody, replacement, retirement, or a messaging Claim-holder fence.
type codexReplacementAccountingFixture struct {
	db       *Store
	protocol *CodexProtocolStore
	state    shimcodex.State
	outputID string
}

func newCodexReplacementAccountingFixture(t *testing.T) *codexReplacementAccountingFixture {
	t.Helper()
	db, state := codexObligationDeliveredFixture(t)
	outputID := state.Delivery.Turns[0].OutputAcceptanceID
	ctx := context.Background()
	p, err := db.CodexProtocolStore(ctx, state.Binding.Session, state.Binding.Operation, shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	e, err := shimcodex.Open(ctx, p, state.Binding, state.Epoch, false, shimcodex.Limits{InboxItems: shimcodex.ProjectionSources, InboxBytes: shimcodex.ProjectionBudget})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.AcceptExit(ctx, "j:3", shim.Exit{Status: 0}); err != nil {
		t.Fatal(err)
	}
	if err = e.AcceptReplayHighWater(ctx, state.Epoch, "j:3"); err != nil {
		t.Fatal(err)
	}
	if err = e.DeliverInbox(ctx, func(_ context.Context, observed shimcodex.State) (shimcodex.Projection, error) {
		return shimcodex.BuildDeliveryProjection(observed)
	}); err != nil {
		t.Fatal(err)
	}
	return &codexReplacementAccountingFixture{db: db, protocol: p, state: e.Snapshot(), outputID: outputID}
}

// Keep the stored private bytes and original public references for assertions
// around the real historical issuer once its callable seam is integrated.
type codexReplacementAccountingSnapshot struct {
	custody         SessionShimRow
	protocolJSON    string
	publicEventJSON string
}

func (f *codexReplacementAccountingFixture) snapshot(t *testing.T) codexReplacementAccountingSnapshot {
	t.Helper()
	var snapshot codexReplacementAccountingSnapshot
	var err error
	snapshot.custody, err = f.db.SessionShim(context.Background(), f.state.Binding.Session)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.db.QueryRow(`SELECT state_json FROM codex_shim_protocol WHERE session_id=? AND shim_key=?`, f.state.Binding.Session, f.state.Binding.Operation).Scan(&snapshot.protocolJSON); err != nil {
		t.Fatal(err)
	}
	if err = f.db.db.QueryRow(`SELECT payload_json FROM events WHERE session_id=? AND kind=? AND json_extract(payload_json,'$.output_id')=?`, f.state.Binding.Session, events.KindSessionTurnOutput, f.outputID).Scan(&snapshot.publicEventJSON); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *codexReplacementAccountingFixture) updateProtocol(t *testing.T, edit func(*shimcodex.State)) {
	t.Helper()
	ctx := context.Background()
	next, err := f.protocol.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := next.Revision
	edit(&next)
	next.Revision = previous + 1
	if err = f.protocol.Commit(ctx, previous, next); err != nil {
		t.Fatal(err)
	}
	f.state = next
}

func TestCodexReplacementAccountingFixturePreservesTerminalDelivery(t *testing.T) {
	f := newCodexReplacementAccountingFixture(t)
	before := f.snapshot(t)
	proof, err := f.db.LoadVerifiedCodexDelivery(context.Background(), f.state, f.state.ReplayHighWater)
	if err != nil || proof == nil {
		t.Fatal("fixture did not earn its actual public delivery receipt", err)
	}
	if err = proof.Validate(f.db, f.state, f.state.ReplayHighWater, true); err != nil {
		t.Fatal("terminal receipt does not cover the frozen protocol", err)
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("receipt validation changed private protocol, custody, or original public references")
	}
}

func TestCodexReplacementAccountingFixtureRetainsUnknownTerminalWork(t *testing.T) {
	f := newCodexReplacementAccountingFixture(t)
	f.updateProtocol(t, func(state *shimcodex.State) {
		state.Operations = append(state.Operations, shimcodex.Operation{
			ID: state.NextID, Method: "turn/start", Params: []byte(`{"threadId":"thread","input":[]}`),
			Phase: shimcodex.Attempted, EffectUnknown: true,
		})
		state.NextID++
	})
	before := f.snapshot(t)
	proof, err := f.db.LoadVerifiedCodexDelivery(context.Background(), f.state, f.state.ReplayHighWater)
	if proof != nil || !errors.Is(err, ErrCodexDeliveryUnsupported) {
		t.Fatal("provider exit settled an unknown private effect", err)
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("refusal changed retained unknown work or original public references")
	}
}

func TestCodexReplacementAccountingRequiresConsumedOldHolderReceipt(t *testing.T) {
	f := newAccountingFixture(t, true)
	if err := f.journal.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	history := &codexReplacementAccountingFixture{
		db: f.store, state: f.state, outputID: f.state.Delivery.Turns[0].OutputAcceptanceID,
	}
	before := history.snapshot(t)
	journalPath := filepath.Join(f.root, "j", "00000001.seg")
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	// The actor recipient has no session binding. Only the real historical
	// attempt holder associates this delivery obligation with the old session.
	actor := messaging.Address{Kind: messaging.KindAgent, Authority: "local", ID: "retained-actor"}
	deliveries := f.store.DeliveryStore()
	enqueued, err := deliveries.Enqueue(ctx, delivery.EnqueueRequest{
		From:       messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "fixture"},
		Recipients: []delivery.RecipientTarget{{Address: actor}},
		Kind:       messaging.MsgKindNotice, Payload: []byte("synthetic retained obligation"),
		ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := deliveries.Claim(ctx, delivery.ClaimRequest{
		DeliveryID: enqueued.Deliveries[0].ID,
		Holder:     f.state.Binding.Session, LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := delivery.LeaseRef{
		DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID,
		LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: claim.Attempt.BindingGeneration,
	}
	for _, stage := range []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted} {
		accepted, _, err := deliveries.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: stage})
		if err != nil {
			t.Fatal(err)
		}
		proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt)
		if proof != nil {
			t.Fatal("unconsumed old-holder obligation earned historical accounting")
		}
		accountingCode(t, err, "ack_pending")
		current, err := deliveries.GetDelivery(ctx, accepted.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current != accepted {
			t.Fatal("accounting refusal altered the outstanding delivery lease")
		}
	}
	consumed, attempt, err := deliveries.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageConsumed})
	if err != nil {
		t.Fatal(err)
	}
	if consumed.Status != delivery.DeliveryDelivered || consumed.ActiveAttemptID != "" || attempt.Stage != delivery.StageConsumed {
		t.Fatal("fixture did not settle through real delivery consumption")
	}
	proof, err := f.store.AccountCodexReplacement(ctx, f.state, f.receipt)
	if err != nil || proof == nil {
		t.Fatal("real consumed receipt did not earn historical accounting", err)
	}
	if proof.Reference().MessagingSHA256 == "" {
		t.Fatal("accounting omitted its messaging evidence reference")
	}
	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = f.store.ValidateCodexReplacementAccountingTx(ctx, tx, proof, f.receipt); err != nil {
		t.Fatal(err)
	}
	// A genuine consumed receipt is removed only within this synthetic write
	// transaction. The issuer must recheck it through that same transaction.
	if _, err = tx.ExecContext(ctx, `DELETE FROM messaging_receipts WHERE delivery_id=? AND attempt_id=? AND stage=?`, lease.DeliveryID, lease.AttemptID, delivery.StageConsumed); err != nil {
		t.Fatal(err)
	}
	accountingCode(t, f.store.ValidateCodexReplacementAccountingTx(ctx, tx, proof, f.receipt), "ack_pending")
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ValidateCodexReplacementAccountingTx(ctx, f.store.db, proof, f.receipt); err != nil {
		t.Fatal("rollback did not preserve genuine consumption evidence", err)
	}
	if after := history.snapshot(t); after != before {
		t.Fatal("historical accounting changed old custody, private ledger, or public acceptance")
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(journalAfter) != string(journalBefore) {
		t.Fatal("historical accounting drained or changed the retained journal")
	}
}
