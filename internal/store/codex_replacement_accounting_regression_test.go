//go:build !windows

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/substrate/harness/shim"
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
