package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
	"github.com/hollis-labs/tether/internal/store"
)

func wakeEnvelope(to messaging.Address, id string) messaging.Envelope {
	env := messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "sender", ID: "author"}, To: to, Metadata: map[string]string{wakeintent.MessageIDKey: id}}
	wakeintent.Put(&env, wakeintent.Intent{WakeText: "read your mailbox", Urgency: "high"})
	return env
}

// Real daemon HTTP handlers and SQLite stores on two owned local listeners;
// only the provider runtime is faked. No production host/configuration touched.
func TestRecipientWakeTwoLocalDaemonsAndRedelivery(t *testing.T) {
	ctx := context.Background()
	receiver, reg := newWakeHarness(t)
	rt := newFakeRuntime()
	rt.setAlive("live", true, agentsessions.LiveStateIdle)
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "receiver", ID: "worker"}
	if err := receiver.CreateSession(store.SessionRow{ID: "live", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	receiver.SetMessageDeliveryObserver(func(ctx context.Context, env messaging.Envelope) string {
		raw, _ := json.Marshal(recipientWake(ctx, receiver, reg, rt.seam(), env))
		return string(raw)
	})
	receiverDaemon := &daemon.Server{Service: &recipientTestLaunchService{}, MessageStore: receiver.MessagingStore()}
	receiverHTTP := httptest.NewServer(receiverDaemon.Handler())
	defer receiverHTTP.Close()
	peer, err := federation.HTTPDialer(receiverHTTP.Client())(federation.Peer{Authority: "receiver", BaseURL: receiverHTTP.URL})
	if err != nil {
		t.Fatal(err)
	}
	sender, err := store.Open(filepath.Join(t.TempDir(), "sender.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	router := federation.NewRouter(sender.MessagingStore(), "sender")
	if err := router.Register("receiver", peer); err != nil {
		t.Fatal(err)
	}
	senderDaemon := &daemon.Server{Service: &recipientTestLaunchService{}, MessageStore: &recipientTestRouterStore{InboxStore: sender.MessagingStore(), router: router}}
	senderHTTP := httptest.NewServer(senderDaemon.Handler())
	defer senderHTTP.Close()
	senderPeer, err := federation.HTTPDialer(senderHTTP.Client())(federation.Peer{Authority: "sender", BaseURL: senderHTTP.URL})
	if err != nil {
		t.Fatal(err)
	}
	env := wakeEnvelope(to, "source-message")
	sent, err := senderPeer.Send(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var outcome api.WakeOutcome
	if err = json.Unmarshal([]byte(sent.Metadata[wakeintent.OutcomeKey]), &outcome); err != nil || !outcome.Attempted || !outcome.Delivered {
		t.Fatalf("wake %+v %v", outcome, err)
	}
	for range 3 {
		replayed, err := senderPeer.Send(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		if replayed.ID != sent.ID {
			t.Fatalf("canonical message changed %s -> %s", sent.ID, replayed.ID)
		}
	}
	if rt.sendCallCount("live") != 1 {
		t.Fatalf("redelivery submitted %d turns", rt.sendCallCount("live"))
	}
	// Response observations are not hashed back into immutable content.
	if _, err = senderPeer.Send(ctx, sent); err != nil {
		t.Fatalf("returned envelope replay: %v", err)
	}
	conflict := env
	wakeintent.Put(&conflict, wakeintent.Intent{WakeText: "changed intent"})
	if _, err = senderPeer.Send(ctx, conflict); err == nil {
		t.Fatal("changed accepted intent was not rejected")
	}
	page, err := receiver.MessagingStore().List(ctx, to, store.ListFilter{Limit: 10})
	if err != nil || page.Total != 1 {
		t.Fatalf("duplicate mail %+v %v", page, err)
	}
}

type recipientTestRouterStore struct {
	store.InboxStore
	router *federation.Router
}

type recipientTestLaunchService struct{ api.LaunchService }

func (s *recipientTestRouterStore) Send(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	return s.router.Send(ctx, env)
}

func TestRecipientWakeConcurrentAdmissionAndCrashUnknown(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s", true, agentsessions.LiveStateIdle)
	to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s"}
	env, err := st.MessagingStore().Send(ctx, wakeEnvelope(to, "one"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = recipientWake(ctx, st, reg, rt.seam(), env) }()
	}
	wg.Wait()
	if rt.sendCallCount("s") != 1 {
		t.Fatalf("concurrent turns %d", rt.sendCallCount("s"))
	}
	crash, err := st.MessagingStore().Send(ctx, wakeEnvelope(to, "crash"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := st.AdmitRecipientWake(ctx, crash.ID)
	if err != nil || !first {
		t.Fatal(first, err)
	}
	// Admission persisted, provider submission not known. This represents
	// either crash BEFORE send or AFTER send/before outcome persistence.
	got := recipientWake(ctx, st, reg, rt.seam(), crash)
	if got.Reason != "wake-outcome-unknown" || got.Delivered || got.Attempted {
		t.Fatalf("uncertain crash guessed %+v", got)
	}
	if rt.sendCallCount("s") != 1 {
		t.Fatal("uncertain admission resubmitted")
	}
}

func TestRecipientWakeNoWakeOfflineAndSubmitFailurePreserveMail(t *testing.T) {
	for _, tc := range []string{"no-wake", "offline", "submit-failed"} {
		t.Run(tc, func(t *testing.T) {
			st, reg := newWakeHarness(t)
			rt := newFakeRuntime()
			ctx := context.Background()
			to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s"}
			env := wakeEnvelope(to, tc)
			if tc == "no-wake" {
				wakeintent.Put(&env, wakeintent.Intent{NoWake: true})
			}
			if tc != "offline" {
				rt.setAlive("s", true, agentsessions.LiveStateIdle)
			}
			if tc == "submit-failed" {
				rt.sendErr["s"] = errors.New("provider rejected turn")
			}
			st.SetMessageDeliveryObserver(func(ctx context.Context, env messaging.Envelope) string {
				raw, _ := json.Marshal(recipientWake(ctx, st, reg, rt.seam(), env))
				return string(raw)
			})
			sent, err := st.MessagingStore().Send(ctx, env)
			if err != nil {
				t.Fatalf("mail failed because wake failed: %v", err)
			}
			if _, err = st.MessagingStore().Get(ctx, sent.ID); err != nil {
				t.Fatal("mail missing", err)
			}
			var got api.WakeOutcome
			if err = json.Unmarshal([]byte(sent.Metadata[wakeintent.OutcomeKey]), &got); err != nil {
				t.Fatal(err)
			}
			if got.Delivered {
				t.Fatalf("unearned wake success %+v", got)
			}
			if tc == "no-wake" {
				if got.Reason != "no-wake" {
					t.Fatal(got)
				}
				if _, err = runWakeSweep(ctx, st, reg, rt.seam(), nil); err != nil {
					t.Fatal(err)
				}
				if rt.sendCallCount("s") != 0 {
					t.Fatal("pump ignored no_wake")
				}
			}
		})
	}
}

func TestRecipientWakeBusyRetryUsesAcceptedIntent(t *testing.T) {
	st, reg := newWakeHarness(t)
	rt := newFakeRuntime()
	ctx := context.Background()
	rt.setAlive("s", true, agentsessions.LiveStateProcessing)
	to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s"}
	st.SetMessageDeliveryObserver(func(ctx context.Context, env messaging.Envelope) string {
		raw, _ := json.Marshal(recipientWake(ctx, st, reg, rt.seam(), env))
		return string(raw)
	})
	env, err := st.MessagingStore().Send(ctx, wakeEnvelope(to, "busy"))
	if err != nil {
		t.Fatal(err)
	}
	if rt.sendCallCount("s") != 0 {
		t.Fatal("busy recipient got turn")
	}
	_ = env
	// Let the supported retry deadline elapse; the test does not mutate the
	// shared delivery library's private tables.
	time.Sleep(wakeBusyRetryBackoff + 20*time.Millisecond)
	rt.setAlive("s", true, agentsessions.LiveStateIdle)
	if _, err = runWakeSweep(ctx, st, reg, rt.seam(), nil); err != nil {
		t.Fatal(err)
	}
	if rt.sendCallCount("s") != 1 {
		t.Fatalf("retry turns %d", rt.sendCallCount("s"))
	}
}

func TestRecipientWakeGeneratedTextUsesAcceptedUrgency(t *testing.T) {
	for _, tc := range []struct {
		name, legacy, intent, want string
	}{
		{"legacy", "low", "", "low"},
		{"accepted-intent", "low", "high", "high"},
		{"default", "", "", "normal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, reg := newWakeHarness(t)
			rt := newFakeRuntime()
			rt.setAlive("s", true, agentsessions.LiveStateIdle)
			env := messaging.Envelope{
				Kind:     messaging.MsgKindNotice,
				From:     messaging.Address{Kind: messaging.KindAgent, Authority: "sender", ID: "author"},
				To:       messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s"},
				Metadata: map[string]string{"urgency": tc.legacy},
			}
			if tc.intent != "" {
				wakeintent.Put(&env, wakeintent.Intent{Urgency: tc.intent})
			}
			accepted, err := st.MessagingStore().Send(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			out := recipientWake(context.Background(), st, reg, rt.seam(), accepted)
			if !out.Delivered {
				t.Fatalf("wake failed: %+v", out)
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.sendCalls) != 1 || !strings.Contains(rt.sendCalls[0].Text, "urgency `"+tc.want+"`") {
				t.Fatalf("accepted urgency did not reach runtime: %+v", rt.sendCalls)
			}
		})
	}
}
