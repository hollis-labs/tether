package broker

import (
	"context"
	"testing"
	"time"
)

func TestDispatcher_WaitAndDeliver(t *testing.T) {
	d := NewDispatcher()
	correlationID := "corr-001"

	// Start waiting for a response with matching correlation ID.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan *Envelope, 1)
	errCh := make(chan error, 1)
	go func() {
		env, err := d.Wait(ctx, correlationID)
		resultCh <- env
		errCh <- err
	}()

	// Small delay, then deliver the response.
	time.Sleep(20 * time.Millisecond)
	resp := &Envelope{
		ID:            "resp-001",
		CorrelationID: correlationID,
		MessageType:   TypeResponse,
	}
	d.Deliver(resp)

	select {
	case env := <-resultCh:
		if env == nil || env.ID != "resp-001" {
			t.Errorf("got %+v, want ID=resp-001", env)
		}
	case <-time.After(3 * time.Second):
		t.Error("Wait did not unblock after Deliver")
	}
	if err := <-errCh; err != nil {
		t.Errorf("Wait returned error: %v", err)
	}
}

func TestDispatcher_Timeout(t *testing.T) {
	d := NewDispatcher()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := d.Wait(ctx, "no-such-corr")
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
}

func TestDispatcher_ConcurrentWaiters(t *testing.T) {
	d := NewDispatcher()
	n := 20
	done := make(chan string, n)

	for i := 0; i < n; i++ {
		corrID := "corr-" + string(rune('A'+i))
		go func(id string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			env, err := d.Wait(ctx, id)
			if err != nil || env == nil {
				done <- "FAIL"
				return
			}
			done <- env.CorrelationID
		}(corrID)
	}

	// Deliver responses in random order.
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < n; i++ {
		id := "corr-" + string(rune('A'+i))
		d.Deliver(&Envelope{ID: "resp-" + id, CorrelationID: id})
	}

	received := make(map[string]bool)
	for i := 0; i < n; i++ {
		select {
		case id := <-done:
			if id == "FAIL" {
				t.Error("one waiter failed")
			} else {
				received[id] = true
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for goroutine %d", i)
		}
	}
	if len(received) != n {
		t.Errorf("got %d distinct results, want %d", len(received), n)
	}
}

func TestDispatcher_DeliverNoWaiter_NoBlock(t *testing.T) {
	d := NewDispatcher()
	// Deliver to a correlation ID nobody is waiting on — should be a no-op.
	done := make(chan struct{})
	go func() {
		d.Deliver(&Envelope{ID: "x", CorrelationID: "orphan"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("Deliver blocked when no waiter present")
	}
}
