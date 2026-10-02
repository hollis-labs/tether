package mcpforward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
)

// Done evaluation marks the waiter's arrival at the pending-attempt select.
type initializationWaiterContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *initializationWaiterContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

// Trigger expiry only after all waiters have joined, without depending on
// scheduler speed to reach that barrier before a wall-clock timeout.
type initializationDeadlineContext struct {
	context.Context
	expired chan struct{}
}

func (c *initializationDeadlineContext) Done() <-chan struct{} { return c.expired }
func (c *initializationDeadlineContext) Err() error {
	select {
	case <-c.expired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestFailedInitializationIsSharedWithWaiters(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) { testFailedInitialization(t, deadline) })
	}
}

func testFailedInitialization(t *testing.T, deadline bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, fail := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Method == "initialize" {
			if attempts.Add(1) == 1 {
				close(started)
			}
			select {
			case <-fail:
			case <-r.Context().Done():
				return
			}
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-fail:
		default:
			close(fail)
		}
	}()
	relay := &daemonSession{client: client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("session"))}
	results := make(chan error, 9)
	owner := ctx
	expired := make(chan struct{})
	var expire sync.Once
	if deadline {
		owner = &initializationDeadlineContext{Context: context.Background(), expired: expired}
		defer expire.Do(func() { close(expired) })
	}
	go func() { _, err := relay.get(owner, nil); results <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for range 8 {
		waiter := &initializationWaiterContext{Context: ctx, joined: make(chan struct{})}
		go func() { _, err := relay.get(waiter, nil); results <- err }()
		select {
		case <-waiter.joined:
		case <-ctx.Done():
			t.Fatal("waiter did not join initialization", ctx.Err())
		}
	}
	// A response failure is event-driven. The deadline variant deliberately
	// hangs the first request until its setup context expires.
	if deadline {
		expire.Do(func() { close(expired) })
	} else {
		close(fail)
	}
	var first error
	for range 9 {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("failed initialize succeeded")
			}
			if first == nil {
				first = err
			}
			if !errors.Is(err, first) {
				t.Fatalf("waiter started a separate attempt: %v / %v", err, first)
			}
		case <-ctx.Done():
			t.Fatal("waiters failed serially", ctx.Err())
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("initialize dispatched %d times", got)
	}
	if deadline {
		if relayError(first).Code != -32004 {
			t.Fatalf("deadline misclassified: %v", first)
		}
		close(fail)
	}
	// A later independent caller can try again after the shared failure.
	if _, err := relay.get(ctx, nil); err == nil {
		t.Fatal("later failed initialize succeeded")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("later caller did not retry: %d", got)
	}
}
