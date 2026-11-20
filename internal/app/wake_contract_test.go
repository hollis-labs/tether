package app

// wake_contract_test.go: the delivery and wake contract a watchdog relies on
// (CW-20261118-0024, docs/messaging-wake-contract.md), driven through the
// send-time observer exactly as the daemon installs it: a send returns the
// stored message with tether.wake_outcome, and only the provider runtime is
// faked.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

type contractRig struct {
	t   *testing.T
	st  *store.Store
	reg *registry.Service
	rt  *fakeRuntime
	sm  wakeRuntime
	to  messaging.Address
}

func newContractRig(t *testing.T, tri *mailTriage) *contractRig {
	t.Helper()
	st, reg := newWakeHarness(t)
	r := &contractRig{t: t, st: st, reg: reg, rt: newFakeRuntime(), to: messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "lead"}}
	r.sm = r.rt.seam()
	if tri != nil {
		tri.st = st
		r.sm.triage = tri
	}
	st.SetMessageDeliveryObserver(func(ctx context.Context, env messaging.Envelope) string {
		raw, _ := json.Marshal(recipientWake(ctx, st, reg, r.sm, env))
		return string(raw)
	})
	return r
}

func (r *contractRig) bind(session string) {
	r.t.Helper()
	if err := r.st.CreateSession(store.SessionRow{ID: session, LogicalAgentID: r.to.ID, State: "running"}, nil); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.reg.LeaseBinding(context.Background(), registry.LogicalAgentBindingTarget(r.to.ID), session, "local", session, nil, "", 0); err != nil {
		r.t.Fatal(err)
	}
}

// watchdogSend is the supported watchdog call: a request with a stable
// idempotency key per episode, returning the stored message and its outcome.
func (r *contractRig) watchdogSend(key string) (messaging.Envelope, api.WakeOutcome) {
	r.t.Helper()
	env := messaging.Envelope{Kind: messaging.MsgKindRequest, From: messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "watchdog"}, To: r.to,
		Payload: json.RawMessage(`{"subject":"idle check","body":"you look idle"}`), Metadata: map[string]string{wakeintent.MessageIDKey: key}}
	wakeintent.Put(&env, wakeintent.Intent{Urgency: "normal"})
	sent, err := r.st.MessagingStore().Send(context.Background(), env)
	if err != nil {
		r.t.Fatal(err)
	}
	var out api.WakeOutcome
	if err := json.Unmarshal([]byte(sent.Metadata[wakeintent.OutcomeKey]), &out); err != nil {
		r.t.Fatalf("send returned no wake outcome: %v", err)
	}
	return sent, out
}

func (r *contractRig) deliveryStatus(id string) delivery.DeliveryStatus {
	r.t.Helper()
	did, ok, err := r.st.DeliveryIDForMessage(context.Background(), id)
	if err != nil || !ok {
		r.t.Fatalf("no delivery for %s: %v", id, err)
	}
	d, err := r.st.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(did))
	if err != nil {
		r.t.Fatal(err)
	}
	return d.Status
}

// Idle, busy, offline: the outcome names the stage reached, mail is stored in
// every case, and only an idle session gets a turn.
func TestWakeContract_IdleBusyOffline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     agentsessions.LiveState
		alive     bool
		attempted bool
		delivered bool
		reason    string
		turns     int
	}{
		{"idle", agentsessions.LiveStateIdle, true, true, true, "", 1},
		{"busy", agentsessions.LiveStateProcessing, true, false, false, "busy", 0},
		{"offline", agentsessions.LiveStateIdle, false, false, false, "offline", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newContractRig(t, nil)
			r.bind("s1")
			r.rt.setAlive("s1", tc.alive, tc.state)
			sent, out := r.watchdogSend("wd:lead:ep1")
			if out.Attempted != tc.attempted || out.Delivered != tc.delivered || out.Reason != tc.reason {
				t.Fatalf("outcome %+v, want attempted=%v delivered=%v reason=%q", out, tc.attempted, tc.delivered, tc.reason)
			}
			if n := r.rt.sendCallCount("s1"); n != tc.turns {
				t.Fatalf("turns %d, want %d", n, tc.turns)
			}
			env, err := r.st.MessagingStore().Get(context.Background(), sent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (env.DeliveredAt != nil) != tc.delivered {
				t.Fatalf("delivered_at=%v outcome=%+v", env.DeliveredAt, out)
			}
			if tc.delivered {
				var count int
				if err := r.st.DB().QueryRow(`SELECT count(*) FROM message_event_journal WHERE kind='message.delivered' AND json_extract(payload_json,'$.message_id')=? AND json_extract(payload_json,'$.path')='wake_hub' AND json_extract(payload_json,'$.session_id')='s1'`, sent.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("hub delivery events=%d err=%v", count, err)
				}
			}
			// Delivered is not completion: the delivery stays leased at
			// turn_submitted until the recipient reads or consumes it.
			if tc.delivered && r.deliveryStatus(sent.ID) == delivery.DeliveryDelivered {
				t.Fatal("a submitted wake settled the delivery before the recipient read it")
			}
		})
	}
}

// Busy is retried by the sweep once the session is idle; offline is retried
// when a session comes online. Both reach exactly one turn, and the stored
// outcome a later duplicate send returns is the delivered one.
func TestWakeContract_BusyAndOfflineRetryToOneTurn(t *testing.T) {
	saved := wakeOfflineRetryBackoff
	wakeOfflineRetryBackoff = 0
	t.Cleanup(func() { wakeOfflineRetryBackoff = saved })
	for _, tc := range []string{"busy", "offline"} {
		t.Run(tc, func(t *testing.T) {
			r := newContractRig(t, nil)
			r.bind("s1")
			if tc == "busy" {
				r.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
			} else {
				r.rt.setAlive("s1", false, agentsessions.LiveStateIdle)
			}
			sent, _ := r.watchdogSend("wd:lead:" + tc)
			if tc == "busy" {
				time.Sleep(wakeBusyRetryBackoff + 20*time.Millisecond)
			}
			r.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			for range 3 {
				if _, err := runWakeSweep(context.Background(), r.st, r.reg, r.sm, nil); err != nil {
					t.Fatal(err)
				}
			}
			if n := r.rt.sendCallCount("s1"); n != 1 {
				t.Fatalf("turns %d, want 1", n)
			}
			again, out := r.watchdogSend("wd:lead:" + tc)
			if again.ID != sent.ID || !out.Delivered {
				t.Fatalf("duplicate send: id %s (want %s), outcome %+v", again.ID, sent.ID, out)
			}
			if n := r.rt.sendCallCount("s1"); n != 1 {
				t.Fatalf("duplicate send woke again: %d turns", n)
			}
		})
	}
}

// A replaced session: the wake goes to the session the actor's current
// binding names, never the older one, even though both are alive.
func TestWakeContract_ReplacedSessionWakesCurrentBinding(t *testing.T) {
	r := newContractRig(t, nil)
	r.bind("old")
	r.rt.setAlive("old", true, agentsessions.LiveStateIdle)
	r.bind("new")
	r.rt.setAlive("new", true, agentsessions.LiveStateIdle)
	_, out := r.watchdogSend("wd:lead:replaced")
	if !out.Delivered || out.SessionID != "new" {
		t.Fatalf("outcome %+v, want delivered to new", out)
	}
	if r.rt.sendCallCount("old") != 0 {
		t.Fatal("replaced session was woken")
	}
}

// Duplicate sends with one idempotency key are one message and at most one
// wake, and every duplicate reports the first send's wake outcome, also on a
// triaged seat whose action cap would otherwise answer "deferred-cap".
func TestWakeContract_DuplicateSendReportsTheStoredOutcome(t *testing.T) {
	for _, triaged := range []bool{false, true} {
		name := "untriaged"
		var tri *mailTriage
		if triaged {
			name = "triaged"
			now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
			tri = &mailTriage{now: func() time.Time { return now }, policy: func(to messaging.Address) seatPolicy {
				return seatPolicy{seat: to.URN(), handle: "@lead", enabled: true, interval: 3 * time.Minute}
			}}
		}
		t.Run(name, func(t *testing.T) {
			r := newContractRig(t, tri)
			r.bind("s1")
			r.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			first, out := r.watchdogSend("wd:lead:dup")
			if !out.Delivered {
				t.Fatalf("first send %+v", out)
			}
			for i := range 3 {
				again, dup := r.watchdogSend("wd:lead:dup")
				if again.ID != first.ID {
					t.Fatalf("duplicate %d stored a second message", i)
				}
				if dup != out {
					t.Fatalf("duplicate %d reported %+v, first send was %+v", i, dup, out)
				}
			}
			if n := r.rt.sendCallCount("s1"); n != 1 {
				t.Fatalf("duplicates woke %d times", n)
			}
		})
	}
}

// mark_read and consume both settle the delivery, so a wake left awaiting
// consumption is never retried as a reminder; list changes nothing.
func TestWakeContract_ReadOrConsumeSettlesTheWake(t *testing.T) {
	for _, how := range []string{"mark_read", "consume"} {
		t.Run(how, func(t *testing.T) {
			r := newContractRig(t, nil)
			r.bind("s1")
			r.rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			sent, out := r.watchdogSend("wd:lead:" + how)
			if !out.Delivered {
				t.Fatalf("wake %+v", out)
			}
			ctx := context.Background()
			if _, err := r.st.MessagingStore().List(ctx, r.to, store.ListFilter{Limit: 10}); err != nil {
				t.Fatal(err)
			}
			if r.deliveryStatus(sent.ID) == delivery.DeliveryDelivered {
				t.Fatal("list settled the delivery")
			}
			var err error
			if how == "mark_read" {
				err = r.st.MessagingStore().MarkRead(ctx, sent.ID, r.to)
			} else {
				err = r.st.MessagingStore().Consume(ctx, sent.ID, r.to)
			}
			if err != nil {
				t.Fatal(err)
			}
			if s := r.deliveryStatus(sent.ID); s != delivery.DeliveryDelivered {
				t.Fatalf("delivery %s after %s, want delivered", s, how)
			}
			if _, err := runWakeSweep(ctx, r.st, r.reg, r.sm, nil); err != nil {
				t.Fatal(err)
			}
			if n := r.rt.sendCallCount("s1"); n != 1 {
				t.Fatalf("settled mail woke again: %d turns", n)
			}
		})
	}
}
