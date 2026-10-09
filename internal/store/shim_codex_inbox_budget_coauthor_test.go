//go:build !windows

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/shimcodex"
)

// No provider or live host is involved. ACKs here only record the private
// transport cursor; they do not certify public delivery or caller consumption.
type codexInboxBudgetTransport struct{ acks []string }

func (*codexInboxBudgetTransport) Validate(context.Context) error { return nil }
func (*codexInboxBudgetTransport) Inject(context.Context, string, []byte) error {
	return errors.New("unexpected provider input in inbox-only fixture")
}
func (*codexInboxBudgetTransport) Replay(context.Context, string) error {
	return errors.New("unexpected live replay in inbox-only fixture")
}
func (t *codexInboxBudgetTransport) Ack(_ context.Context, cursor string) error {
	t.acks = append(t.acks, cursor)
	return nil
}
func (*codexInboxBudgetTransport) Close() error { return nil }

func TestCodexInboxNearCapBatchIsAtomicAndRetainedOnReopen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backlog int
		batch   int
		fits    bool
	}{
		{name: "fills_capacity", backlog: 977, batch: 47, fits: true},
		{name: "exceeds_capacity", backlog: 977, batch: 48},
		{name: "already_at_capacity", backlog: 1024, batch: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, state := codexDeliverySQLFixture(t)
			port, err := db.CodexProtocolStore(ctx, state.Binding.Session, state.Binding.Operation, shimcodex.ProjectionBudget)
			if err != nil {
				t.Fatal(err)
			}
			// Model answered input with an outstanding private output inbox.
			// There is no new turn submission or native output acceptance.
			state.Revision++
			state.InitializeID = shimcodex.FirstID
			state.Initialized = true
			state.ThreadID = "fixture-thread"
			state.LastTerminal = "fixture-turn"
			state.NextID = shimcodex.FirstID + 4
			for i, method := range []string{"initialize", "initialized", "thread/start", "turn/start"} {
				state.Operations = append(state.Operations, shimcodex.Operation{
					ID: shimcodex.FirstID + uint64(i), Method: method,
					Params: json.RawMessage(`{}`), Notification: method == "initialized", Phase: shimcodex.Answered,
				})
			}
			if err = port.Commit(ctx, state.Revision-1, state); err != nil {
				t.Fatal(err)
			}
			limits := shimcodex.Limits{InboxItems: 1024, InboxBytes: shimcodex.ProjectionBudget}
			engine, err := shimcodex.Open(ctx, port, state.Binding, state.Epoch, false, limits)
			if err != nil {
				t.Fatal(err)
			}
			transport := &codexInboxBudgetTransport{}
			session, err := shimcodex.NewSession(engine, transport, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Stop(ctx) })
			line := `{"method":"account/updated","params":{}}` + "\n"
			if err = session.AcceptOutput(ctx, "j:2", "stdout", []byte(strings.Repeat(line, 976))); err != nil {
				t.Fatal(err)
			}
			nextCursor := "j:3"
			if tc.backlog == 1024 {
				if err = session.AcceptOutput(ctx, "j:3", "stdout", []byte(strings.Repeat(line, 47))); err != nil {
					t.Fatal(err)
				}
				nextCursor = "j:4"
			}
			before, err := port.Load(ctx)
			if err != nil || len(before.Inbox) != tc.backlog || before.Delivery != nil {
				t.Fatalf("private backlog not established: error=%v", err)
			}
			ackCount := len(transport.acks)
			frozen, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			err = session.AcceptOutput(ctx, nextCursor, "stdout", []byte(strings.Repeat(line, tc.batch)))
			if tc.fits {
				if err != nil {
					t.Fatal("exactly-full batch refused", err)
				}
			} else if !shimcodex.HasCode(err, "pressure_retained") {
				t.Fatal("over-cap batch did not retain pressure", err)
			}
			after, err := port.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.fits {
				if len(after.Inbox) != 1024 || after.Cursor != nextCursor || after.Revision != before.Revision+1 || len(transport.acks) != ackCount+1 || transport.acks[ackCount] != nextCursor {
					t.Fatal("accepted batch split its private inbox/cursor/ACK transaction")
				}
				if !reflect.DeepEqual(after.Inbox[:len(before.Inbox)], before.Inbox) || !reflect.DeepEqual(after.Operations, before.Operations) {
					t.Fatal("accepted batch replaced outstanding obligations or answered input")
				}
			} else {
				persisted, err := json.Marshal(after)
				if err != nil || !bytes.Equal(persisted, frozen) || len(transport.acks) != ackCount || transport.acks[ackCount-1] != before.Cursor {
					t.Fatal("refused batch mutated durable obligations or acknowledged unaccepted bytes", err)
				}
				inMemory, err := json.Marshal(engine.Snapshot())
				if err != nil || !bytes.Equal(inMemory, frozen) {
					t.Fatal("refused batch left a partial in-memory transition", err)
				}
			}
			if after.Delivery != nil {
				t.Fatal("private batch acceptance minted public delivery")
			}
			if _, err = db.LoadVerifiedCodexDelivery(ctx, after, after.Cursor); !errors.Is(err, ErrCodexDeliveryUnsupported) {
				t.Fatal("undelivered inbox earned a delivery receipt", err)
			}
			reopened, err := shimcodex.Open(ctx, port, after.Binding, after.Epoch+1, false, limits)
			if err != nil {
				t.Fatal("same-provider backlog could not reopen", err)
			}
			snapshot := reopened.Snapshot()
			expected := after
			expected.Epoch++
			expected.Revision++
			expected.ReplayHighWater = ""
			want, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(snapshot)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("reopen changed inbox, answered input or placement identity", err)
			}
		})
	}
}
