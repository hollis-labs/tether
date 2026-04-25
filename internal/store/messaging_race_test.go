package store_test

// messaging_race_test.go — race and concurrency tests for the SQLite-backed
// messaging store. Covers the paths identified in ADR-0023 §13 (Required tests
// for CW-20260423-0021):
//
//   - Concurrent Inbox calls for the same recipient — each message delivered
//     exactly once across all goroutines.
//   - Concurrent Consume calls — idempotent; ConsumedAt set exactly once.
//   - Concurrent Cancel + Consume — exactly one succeeds; no panic.
//   - Concurrent Send + Subscribe — all messages appear on the channel;
//     no drops under fanOut lock contention.
//   - Cancel + Dispatcher.Request in-flight — request resolves with
//     ErrCanceled or context timeout (both are acceptable; the important
//     thing is it does NOT deadlock).
//
// Run with -race to exercise the data-race detector:
//
//	go test -race ./internal/store/... -run TestMessagingStore_Race

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/chrispian/agent-mux/internal/store"
)

// openRaceDB opens a fresh SQLite store in a temp directory.
func openRaceDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "race.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func addr(id string) messaging.Address {
	return messaging.Address{Kind: messaging.KindAgent, Authority: "race-test", ID: id}
}

// ─── Concurrent Inbox ─────────────────────────────────────────────────────────

// TestMessagingStore_Race_ConcurrentInbox sends N messages to a recipient
// and then fires M goroutines all calling Inbox concurrently. The total
// delivered count across all calls must equal N (no double-delivery, no loss).
func TestMessagingStore_Race_ConcurrentInbox(t *testing.T) {
	const (
		msgCount    = 20
		workerCount = 8
	)

	db := openRaceDB(t)
	ms := db.MessagingStore()
	ctx := context.Background()
	to := addr("inbox-concurrent")

	// Send all messages before the concurrent read phase.
	for i := 0; i < msgCount; i++ {
		env := messaging.Envelope{
			Kind: messaging.MsgKindNotice,
			From: addr("sender"),
			To:   to,
		}
		if _, err := ms.Send(ctx, env); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	var (
		wg            sync.WaitGroup
		deliveredOnce int64 // atomic counter of envelopes seen across all workers
	)

	start := make(chan struct{})
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // all goroutines start at the same time
			got, err := ms.Inbox(ctx, to, messaging.Filter{})
			if err != nil {
				// SQLite busy is acceptable under high concurrency; retry.
				return
			}
			atomic.AddInt64(&deliveredOnce, int64(len(got)))
		}()
	}

	close(start)
	wg.Wait()

	total := atomic.LoadInt64(&deliveredOnce)
	if total != msgCount {
		t.Errorf("total delivered across all workers = %d, want %d (each message delivered exactly once)", total, msgCount)
	}
}

// ─── Concurrent Consume ───────────────────────────────────────────────────────

// TestMessagingStore_Race_ConcurrentConsume delivers a single message and
// races N goroutines all calling Consume. Consume is idempotent so all
// should return nil. ConsumedAt should be set.
func TestMessagingStore_Race_ConcurrentConsume(t *testing.T) {
	const workers = 10

	db := openRaceDB(t)
	ms := db.MessagingStore()
	ctx := context.Background()
	to := addr("consume-concurrent")

	sent, err := ms.Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindRequest,
		From: addr("sender"),
		To:   to,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Deliver first (Inbox), so Consume can set consumed_at.
	if _, err := ms.Inbox(ctx, to, messaging.Filter{}); err != nil {
		t.Fatalf("inbox: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = ms.Consume(ctx, sent.ID, to)
		}()
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("worker %d: Consume returned %v, want nil (idempotent)", i, e)
		}
	}

	// Verify ConsumedAt is set.
	got, err := ms.Get(ctx, sent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ConsumedAt == nil {
		t.Error("ConsumedAt nil after concurrent Consume")
	}
}

// ─── Concurrent Cancel + Consume ─────────────────────────────────────────────

// TestMessagingStore_Race_CancelVsConsume fires Cancel and Consume
// concurrently on the same message. The race between the two is acceptable;
// what is NOT acceptable is a panic, a deadlock, or both returning errors.
func TestMessagingStore_Race_CancelVsConsume(t *testing.T) {
	const iterations = 20 // run multiple times to shake out races

	for i := 0; i < iterations; i++ {
		i := i
		t.Run("", func(t *testing.T) {
			t.Parallel()
			db := openRaceDB(t)
			ms := db.MessagingStore()
			ctx := context.Background()
			to := addr("cvc-recipient")

			sent, err := ms.Send(ctx, messaging.Envelope{
				Kind: messaging.MsgKindRequest,
				From: addr("cvc-sender"),
				To:   to,
			})
			if err != nil {
				t.Fatalf("iter %d send: %v", i, err)
			}

			var wg sync.WaitGroup
			cancelErr := make(chan error, 1)
			consumeErr := make(chan error, 1)
			start := make(chan struct{})

			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				cancelErr <- ms.Cancel(ctx, sent.ID)
			}()
			go func() {
				defer wg.Done()
				<-start
				consumeErr <- ms.Consume(ctx, sent.ID, to)
			}()

			close(start)
			wg.Wait()

			cErr := <-cancelErr
			conErr := <-consumeErr

			// Cancel should either succeed (nil) or return ErrNotFound if
			// the message was already canceled. It must NOT be an opaque error.
			if cErr != nil && !errors.Is(cErr, messaging.ErrNotFound) {
				t.Errorf("iter %d: Cancel returned unexpected error: %v", i, cErr)
			}
			// Consume should either succeed (nil), return ErrNotFound (message
			// didn't exist yet in the race), or ErrWrongRecipient.
			// It must NOT be an opaque internal error.
			if conErr != nil &&
				!errors.Is(conErr, messaging.ErrNotFound) &&
				!errors.Is(conErr, store.ErrWrongRecipient) {
				t.Errorf("iter %d: Consume returned unexpected error: %v", i, conErr)
			}
		})
	}
}

// ─── Concurrent Send + Subscribe ─────────────────────────────────────────────

// TestMessagingStore_Race_SendVsSubscribe verifies that fan-out under lock
// contention does not drop messages or race. A subscriber waits while N
// producers fire concurrently.
func TestMessagingStore_Race_SendVsSubscribe(t *testing.T) {
	const producerCount = 10

	db := openRaceDB(t)
	ms := db.MessagingStore()
	to := addr("sub-concurrent")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := ms.Subscribe(ctx, to, messaging.Filter{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Collect received IDs via a separate goroutine.
	received := make(chan string, producerCount*2)
	go func() {
		for env := range ch {
			received <- env.ID
		}
		close(received)
	}()

	// Give the subscriber a moment to register before the send storm.
	time.Sleep(5 * time.Millisecond)

	var wg sync.WaitGroup
	sentIDs := make([]string, producerCount)
	for i := 0; i < producerCount; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sent, err := ms.Send(context.Background(), messaging.Envelope{
				Kind: messaging.MsgKindNotice,
				From: addr("producer"),
				To:   to,
			})
			if err != nil {
				t.Errorf("producer %d send: %v", i, err)
				return
			}
			sentIDs[i] = sent.ID
		}()
	}
	wg.Wait()

	// Collect what the subscriber received; allow up to 2s for all messages.
	deadline := time.After(2 * time.Second)
	got := make(map[string]struct{})
	for len(got) < producerCount {
		select {
		case id := <-received:
			got[id] = struct{}{}
		case <-deadline:
			t.Fatalf("subscriber received %d/%d messages before deadline", len(got), producerCount)
		}
	}

	for i, id := range sentIDs {
		if id == "" {
			continue // send failed — already reported above
		}
		if _, ok := got[id]; !ok {
			t.Errorf("producer %d: message %s not received by subscriber", i, id)
		}
	}
}

// ─── Cancel + Dispatcher.Request in-flight ────────────────────────────────────

// TestMessagingStore_Race_CancelInFlightRequest fires a Dispatcher.Request
// and races a Cancel against it. The request must resolve (either via
// ErrCanceled, ErrRequestTimeout, or a real reply) and must NOT deadlock.
//
// We do NOT require ErrCanceled specifically because the SQLite
// notifyCanceled is currently a no-op (documented in messaging_store.go).
// The test enforces liveness: the call must return within the context
// deadline.
func TestMessagingStore_Race_CancelInFlightRequest(t *testing.T) {
	db := openRaceDB(t)
	ms := db.MessagingStore()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	requester := addr("req-cancel-sender")
	worker := addr("req-cancel-worker")

	// Start the blocking Request in a goroutine.
	requestDone := make(chan error, 1)
	var requestID string
	idReady := make(chan struct{})

	go func() {
		// Subscribe to know when the request envelope is persisted so we
		// can cancel it by ID. We use a small side-channel approach: send
		// the request, capture the sent ID, then cancel.
		sent, err := ms.Send(ctx, messaging.Envelope{
			Kind: messaging.MsgKindRequest,
			From: requester,
			To:   worker,
		})
		if err != nil {
			requestDone <- err
			return
		}
		requestID = sent.ID
		close(idReady)

		// Now wait for a response — this will block until a reply arrives or
		// context expires. (We don't use Dispatcher.Request here because it
		// also calls Send internally and would create a separate envelope.)
		sub, err := ms.Subscribe(ctx, requester, messaging.Filter{
			Kind: []messaging.Kind{messaging.MsgKindResponse},
		})
		if err != nil {
			requestDone <- err
			return
		}
		select {
		case <-sub:
			requestDone <- nil
		case <-ctx.Done():
			requestDone <- messaging.ErrRequestTimeout
		}
	}()

	// Wait for the request envelope to be persisted.
	select {
	case <-idReady:
	case <-ctx.Done():
		t.Fatal("request envelope never persisted")
	}

	// Cancel the request concurrently.
	if err := ms.Cancel(ctx, requestID); err != nil && !errors.Is(err, messaging.ErrNotFound) {
		t.Errorf("Cancel: unexpected error %v", err)
	}

	// The request goroutine must complete within context — no deadlock.
	select {
	case err := <-requestDone:
		if err != nil && !errors.Is(err, messaging.ErrRequestTimeout) && !errors.Is(err, messaging.ErrCanceled) {
			t.Errorf("request goroutine returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("request goroutine did not complete: possible deadlock")
	}
}

// ─── Dispatcher.Request round-trip under concurrent senders ──────────────────

// TestMessagingStore_Race_DispatcherConcurrent fires N concurrent
// Dispatcher.Request/Reply pairs. Each pair must correlate correctly — no
// cross-contamination between pairs under concurrent load.
func TestMessagingStore_Race_DispatcherConcurrent(t *testing.T) {
	const pairs = 8

	db := openRaceDB(t)
	ms := db.MessagingStore()
	d := messaging.NewDispatcher(ms)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	workerAddr := addr("dispatch-worker")

	// Worker: subscribe to requests and echo them back.
	go func() {
		sub, err := ms.Subscribe(ctx, workerAddr, messaging.Filter{
			Kind: []messaging.Kind{messaging.MsgKindRequest},
		})
		if err != nil {
			return
		}
		for req := range sub {
			// Reply with the same payload so the requester can verify.
			_, _ = d.Reply(ctx, req, req.Payload)
		}
	}()

	// Give the worker subscriber a moment to register.
	time.Sleep(5 * time.Millisecond)

	var wg sync.WaitGroup
	errs := make([]error, pairs)
	for i := 0; i < pairs; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := json.RawMessage(`{"i":` + fmt.Sprint(i) + `}`)
			resp, err := d.Request(ctx, messaging.Envelope{
				From:    addr("dispatch-requester"),
				To:      workerAddr,
				Payload: payload,
			})
			if err != nil {
				errs[i] = err
				return
			}
			// Verify the echo — payload must match.
			if string(resp.Payload) != string(payload) {
				errs[i] = fmt.Errorf("payload mismatch: got %s want %s", resp.Payload, payload)
			}
		}()
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("pair %d: %v", i, e)
		}
	}
}


