package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261118-0050: messaging noise rules.

func noneUpdate(to string) envelope.Envelope {
	return envelope.Envelope{Schema: envelope.Schema, Kind: envelope.KindUpdate, To: []string{to}, Attention: envelope.AttentionNone, Subject: "CW-20261118-0022 progress", Body: "status"}
}

// Replay of 2026-11-18 22:44:30Z: six attention:none updates for CW-20261118-0022
// to six different seats woke all six. With triage off (the live default) and on,
// none may wake; each stays unread for the next real wake.
func TestReplayAttentionNoneBurstWakesNobody(t *testing.T) {
	for _, triaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("triage=%v", triaged), func(t *testing.T) {
			r := newTriageRig(t)
			r.tri.policy = func(to messaging.Address) seatPolicy {
				return seatPolicy{seat: to.URN(), handle: "@" + to.ID, enabled: triaged, interval: 3 * time.Minute}
			}
			var envs []messaging.Envelope
			for i := 0; i < 6; i++ {
				id := fmt.Sprintf("seat%d", i)
				if err := r.st.CreateSession(store.SessionRow{ID: "s-" + id, LogicalAgentID: id, State: "running"}, nil); err != nil {
					t.Fatal(err)
				}
				r.rt.setAlive("s-"+id, true, agentsessions.LiveStateIdle)
				to := messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: id}
				r.to = to
				envs = append(envs, r.send(peer, messaging.MsgKindNotice, envelope.ContentType, noneUpdate("@"+id), nil))
			}
			for _, e := range envs {
				if reason := recipientWake(context.Background(), r.st, nil, r.seam, e).Reason; reason == "" || reason == "offline" {
					t.Fatalf("%s: reason %q, want a quiet no-wake", e.ID, reason)
				}
			}
			if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 6; i++ {
				if n := r.rt.sendCallCount(fmt.Sprintf("s-seat%d", i)); n != 0 {
					t.Fatalf("seat%d woke %d times for attention:none", i, n)
				}
			}
		})
	}
}

// attention:none yields to the action rules: an operator, urgency high or a
// reply to the seat's own mail still wakes, with triage off.
func TestAttentionNoneKeepsActionOverrides(t *testing.T) {
	r := newTriageRig(t)
	r.tri.policy = func(messaging.Address) seatPolicy { return seatPolicy{seat: r.to.URN(), handle: "@worker"} }
	hi := r.send(peer, messaging.MsgKindNotice, envelope.ContentType, noneUpdate("@worker"), map[string]string{"urgency": "high"})
	if reason := r.wake(hi); reason != "" {
		t.Fatalf("urgency high attention:none: %q", reason)
	}
	ask := r.sendFrom(r.to.URN(), peer, messaging.MsgKindRequest, "", map[string]string{"subject": "q"})
	raw, _ := json.Marshal(noneUpdate("@worker"))
	f, _ := messaging.ParseURN(peer)
	reply, err := r.st.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: f, To: r.to, InReplyTo: ask.ID, ContentType: envelope.ContentType, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	if reason := r.wake(reply); reason != "" {
		t.Fatalf("attention:none reply to own ask must still wake the asker: %q", reason)
	}
}

// A burst of N messages to one seat with one real ask: one wake, and the rest
// ride along (no later wake repeats them).
func TestBurstWithOneAskWakesOnce(t *testing.T) {
	for _, triaged := range []bool{true} {
		r := newTriageRig(t)
		var all []messaging.Envelope
		for i := 0; i < 8; i++ {
			all = append(all, r.send(peer, messaging.MsgKindNotice, envelope.ContentType, noneUpdate("@worker"), nil))
			all = append(all, r.send(peer, messaging.MsgKindStatusUpdate, "", map[string]string{"subject": fmt.Sprintf("fyi %d", i)}, nil))
		}
		ask := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"subject": "the ask"}, nil)
		all = append(all, ask)
		for _, e := range all {
			r.wake(e)
		}
		for i := 0; i < 3; i++ {
			r.now = r.now.Add(5 * time.Minute)
			if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
				t.Fatal(err)
			}
		}
		if n := r.rt.sendCallCount("live"); n != 1 {
			t.Fatalf("burst of %d with one ask produced %d wakes (triaged=%v), want 1", len(all), n, triaged)
		}
	}
}

// A recipient that is no catalog seat (registry-only agent) keeps its old wake
// behavior: the attention:none rule is for catalog seats.
func TestAttentionNoneWakesNonCatalogRecipient(t *testing.T) {
	r := newTriageRig(t)
	r.tri.policy = func(messaging.Address) seatPolicy { return seatPolicy{} }
	e := r.send(peer, messaging.MsgKindNotice, envelope.ContentType, noneUpdate("@worker"), nil)
	if reason := r.wake(e); reason != "" || r.rt.sendCallCount("live") != 1 {
		t.Fatalf("non-catalog recipient: reason %q, %d turns; want the old wake", reason, r.rt.sendCallCount("live"))
	}
}
