//go:build !windows

package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/store"
)

func codexCompletedInboxFixture(t *testing.T, split ...bool) (*Service, *store.CodexProtocolStore, *shimcodex.Engine) {
	t.Helper()
	s, r, p := codexOutputFixture(t)
	s.Bus = events.NewBus(events.BusOptions{Persister: s.Store})
	updateCodexOutputFixture(t, p, func(state *shimcodex.State) {
		state.Initialized = true
		state.InitializeID = shimcodex.FirstID
		state.NextID = shimcodex.FirstID + 1
		state.ThreadID = "thread"
		state.Operations = []shimcodex.Operation{{ID: shimcodex.FirstID, Method: "initialize", Params: []byte(`{}`), Phase: shimcodex.Answered, Result: []byte(`{}`)}}
	})
	e, err := shimcodex.Open(context.Background(), p, codexBinding(r), 1, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: codexProtocolBudget})
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"emittedAtMs":1,"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"active"}}}`,
		`{"emittedAtMs":2,"method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn","status":"inProgress","items":[],"itemsView":null,"error":null,"startedAt":1,"completedAt":null,"durationMs":null}}}`,
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","startedAtMs":2,"item":{"id":"user","type":"userMessage","clientId":null,"content":[]}}}`,
		`{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","completedAtMs":3,"item":{"id":"user","type":"userMessage","clientId":null,"content":[]}}}`,
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","startedAtMs":3,"item":{"id":"answer","type":"agentMessage","text":"","phase":"final_answer","delivery":null,"memoryCitation":null,"questions":[]}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","itemId":"answer","delta":"hello 世界"}}`,
		`{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","completedAtMs":4,"item":{"id":"answer","type":"agentMessage","text":"hello 世界","phase":"final_answer","delivery":null,"memoryCitation":null,"questions":[]}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed","items":[],"itemsView":null,"error":null,"startedAt":1,"completedAt":4,"durationMs":3}}}`,
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	cursor := "j:1"
	if len(split) != 0 && split[0] {
		// One frame ends with durable carry after complete messages; the
		// next contains no complete message. Reopen before completing it.
		cut := strings.Index(string(data), `"delta":"hello`) + len(`"delta":"hel`)
		for i, chunk := range [][]byte{data[:cut], data[cut : cut+1]} {
			if _, err = e.AcceptOutput(context.Background(), 1, []string{"j:1", "j:2"}[i], "stdout", chunk); err != nil {
				t.Fatal(err)
			}
			if err = e.DeliverInbox(context.Background(), s.deliverCodexInbox); err != nil {
				t.Fatal(err)
			}
			if len(outputEvents(t, s)) != 0 {
				t.Fatal("partial turn was published")
			}
		}
		e, err = shimcodex.Open(context.Background(), p, e.Snapshot().Binding, 2, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: codexProtocolBudget})
		if err != nil {
			t.Fatal(err)
		}
		data, cursor = data[cut+1:], "j:3"
	}
	if _, err = e.AcceptOutput(context.Background(), e.Snapshot().Epoch, cursor, "stdout", data); err != nil {
		t.Fatal(err)
	}
	if err = e.AcceptReplayHighWater(context.Background(), e.Snapshot().Epoch, cursor); err != nil {
		t.Fatal(err)
	}
	return s, p, e
}

func TestHostedCodexDeliveryFragmentedCarrySurvivesReopen(t *testing.T) {
	s, _, e := codexCompletedInboxFixture(t, true)
	ctx := context.Background()
	if err := e.DeliverInbox(ctx, s.deliverCodexInbox); err != nil {
		t.Fatal(err)
	}
	after := e.Snapshot()
	got := outputEvents(t, s)
	if len(got) != 1 || got[0].Text != "hello 世界" || len(after.Inbox) != 0 || len(after.Partial) != 0 {
		t.Fatal("fragmented output was lost, duplicated or retained")
	}
	proof, err := s.Store.LoadVerifiedCodexDelivery(ctx, after, "j:3")
	if err != nil || proof.Validate(s.Store, after, "j:3", false) != nil {
		t.Fatal("fragment coverage did not earn exact delivery proof", err)
	}
}

func TestHostedCodexDeliveryExitDoesNotSettleAnOpenTurn(t *testing.T) {
	s, _, e := codexCompletedInboxFixture(t)
	ctx := context.Background()
	if err := e.DeliverInbox(ctx, s.deliverCodexInbox); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AcceptOutput(ctx, 1, "j:2", "stdout", []byte("{\"method\":\"turn/started\",\"params\":{\"threadId\":\"thread\",\"turn\":{\"id\":\"unfinished\",\"status\":\"inProgress\"}}}\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.AcceptExit(ctx, "j:3", shim.Exit{Status: 7}); err != nil {
		t.Fatal(err)
	}
	before := e.Snapshot()
	if err := e.DeliverInbox(ctx, s.deliverCodexInbox); !shimcodex.HasCode(err, "output_unsupported") {
		t.Fatal("exit certified an unfinished output turn", err)
	}
	after := e.Snapshot()
	if after.Revision != before.Revision || len(after.Inbox) != len(before.Inbox) || len(outputEvents(t, s)) != 1 {
		t.Fatal("unfinished output was drained or published")
	}
	if _, err := s.Store.LoadVerifiedCodexDelivery(ctx, after, "j:1"); err == nil {
		t.Fatal("unfinished exit earned delivery proof")
	}
}

func TestHostedCodexDeliveryStagesBeforeAtomicDrainAndReplaysCrash(t *testing.T) {
	s, p, e := codexCompletedInboxFixture(t)
	ctx := context.Background()
	before := e.Snapshot()
	if _, err := s.Store.LoadVerifiedCodexDelivery(ctx, before, "j:1"); !errors.Is(err, store.ErrCodexDeliveryUnsupported) {
		t.Fatal("private acceptance earned receipt", err)
	}
	if _, err := s.Store.DB().Exec(`CREATE TRIGGER fail_delivery BEFORE UPDATE ON codex_shim_protocol WHEN json_extract(NEW.state_json,'$.delivery') IS NOT NULL BEGIN SELECT RAISE(FAIL,'fixture receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.DeliverInbox(ctx, s.deliverCodexInbox); !shimcodex.HasCode(err, "checkpoint_unknown") {
		t.Fatal("failed receipt commit was certain", err)
	}
	retained, err := p.Load(ctx)
	if err != nil || retained.Revision != before.Revision || len(retained.Inbox) != len(before.Inbox) {
		t.Fatal("failed receipt lost source", err)
	}
	if got := outputEvents(t, s); len(got) != 1 || got[0].Text != "hello 世界" {
		t.Fatal("actual durable output missing")
	}
	if _, err = s.Store.DB().Exec(`DROP TRIGGER fail_delivery`); err != nil {
		t.Fatal(err)
	}
	recovered, err := shimcodex.Open(ctx, p, before.Binding, 2, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: codexProtocolBudget})
	if err != nil {
		t.Fatal(err)
	}
	if err = recovered.AcceptReplayHighWater(ctx, 2, "j:1"); err != nil {
		t.Fatal(err)
	}
	if err = recovered.DeliverInbox(ctx, s.deliverCodexInbox); err != nil {
		t.Fatal(err)
	}
	after := recovered.Snapshot()
	if len(after.Inbox) != 0 || after.Delivery == nil || len(outputEvents(t, s)) != 1 {
		t.Fatal("replay duplicated public output or did not drain")
	}
	proof, err := s.Store.LoadVerifiedCodexDelivery(ctx, after, "j:1")
	if err != nil || proof.Validate(s.Store, after, "j:1", false) != nil {
		t.Fatal("committed delivery did not earn proof", err)
	}
	// A standard protocol commit cannot manufacture a replacement receipt.
	forged := recovered.Snapshot()
	forged.Revision++
	forged.Delivery.Turns[0].OutputAcceptanceID = "forged"
	if err = p.Commit(ctx, after.Revision, forged); err == nil {
		t.Fatal("ordinary Commit minted receipt")
	}
	if err = recovered.AcceptExit(ctx, "j:2", shim.Exit{Status: 0}); err != nil {
		t.Fatal(err)
	}
	if err = proof.Validate(s.Store, recovered.Snapshot(), "j:1", true); err == nil {
		t.Fatal("old receipt certified new exit")
	}
	if err = recovered.DeliverInbox(ctx, s.deliverCodexInbox); err != nil {
		t.Fatal(err)
	}
	terminal := recovered.Snapshot()
	proof, err = s.Store.LoadVerifiedCodexDelivery(ctx, terminal, "j:1")
	if err != nil || proof.Validate(s.Store, terminal, "j:1", true) != nil {
		t.Fatal("journaled exit did not settle", err)
	}
}

func TestHostedCodexDeliveryRetainsUnsupportedOutput(t *testing.T) {
	s, p, e := codexCompletedInboxFixture(t)
	if _, err := e.AcceptOutput(context.Background(), 1, "j:2", "stdout", []byte(`{"method":"future/output","params":{"text":"retained"}}`+"\n")); err != nil {
		t.Fatal(err)
	}
	before := e.Snapshot()
	if err := e.DeliverInbox(context.Background(), s.deliverCodexInbox); !shimcodex.HasCode(err, "output_unsupported") {
		t.Fatal("unknown output was discarded", err)
	}
	after, err := p.Load(context.Background())
	if err != nil || after.Revision != before.Revision || len(after.Inbox) != len(before.Inbox) || len(outputEvents(t, s)) != 0 {
		t.Fatal("unsupported source changed delivery", err)
	}
}

func TestHostedCodexDeliveryConcurrentReaderRetainsInboxAndReusesOutput(t *testing.T) {
	s, _, e := codexCompletedInboxFixture(t)
	ctx := context.Background()
	err := e.DeliverInbox(ctx, func(ctx context.Context, state shimcodex.State) (shimcodex.Projection, error) {
		p, err := s.deliverCodexInbox(ctx, state)
		if err != nil {
			return p, err
		}
		if err = e.AcceptMetadata(ctx, state.Epoch, "j:2", "shim.attached", []byte(`{}`)); err != nil {
			return p, err
		}
		return p, nil
	})
	if !shimcodex.HasCode(err, "delivery_changed") || len(e.Snapshot().Inbox) == 0 {
		t.Fatal("concurrent reader erased source", err)
	}
	if err = e.DeliverInbox(ctx, s.deliverCodexInbox); err != nil {
		t.Fatal(err)
	}
	if len(outputEvents(t, s)) != 1 || len(e.Snapshot().Inbox) != 0 {
		t.Fatal("concurrent replay duplicated output")
	}
	first := e.Snapshot()
	if err = e.AcceptMetadata(ctx, first.Epoch, "j:3", "shim.attached", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err = e.DeliverInbox(ctx, s.deliverCodexInbox); err != nil {
		t.Fatal(err)
	}
	compacted := e.Snapshot()
	if compacted.Delivery.BaseSourceCursor != "j:2" || compacted.Delivery.BaseReceiptSHA256 == "" || len(compacted.Delivery.Sources) != 1 || len(compacted.Delivery.Turns) != 0 {
		t.Fatal("settled source inventory accumulated forever")
	}
	if _, err = s.Store.LoadVerifiedCodexDelivery(ctx, compacted, "j:1"); err != nil {
		t.Fatal("compaction lost committed receipt chain", err)
	}
}

func TestHostedCodexDeliveryStageFailureRetainsSourceUntilRetry(t *testing.T) {
	s, p, e := codexCompletedInboxFixture(t)
	ctx := context.Background()
	before := e.Snapshot()
	if _, err := s.Store.DB().Exec(`CREATE TRIGGER fail_stage BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL,'fixture stage failure'); END`); err != nil {
		t.Fatal(err)
	}
	// The isolated source fixture selects a route for this failure branch.
	if _, err := s.Store.DB().Exec(`UPDATE sessions SET route_json='{"channel":"ops","kinds":["final"]}' WHERE id='codex'`); err != nil {
		t.Fatal(err)
	}
	if err := e.DeliverInbox(ctx, s.deliverCodexInbox); err == nil {
		t.Fatal("failed stage earned drain")
	}
	after, err := p.Load(ctx)
	if err != nil || after.Revision != before.Revision || len(after.Inbox) != len(before.Inbox) || len(outputEvents(t, s)) != 0 {
		t.Fatal("failed stage lost source", err)
	}
	if _, err = s.Store.DB().Exec(`DROP TRIGGER fail_stage`); err != nil {
		t.Fatal(err)
	}
	if err = e.DeliverInbox(ctx, s.deliverCodexInbox); err != nil {
		t.Fatal(err)
	}
	if len(outputEvents(t, s)) != 1 || len(e.Snapshot().Inbox) != 0 {
		t.Fatal("stage retry did not settle actual delivery")
	}
}
