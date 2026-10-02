package mcpforward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	go func() { _, err := relay.get(ctx, nil); results <- err }()
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
	if !deadline {
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

func TestInitializationInitiatorCancellationDoesNotFailWaiters(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := make(chan struct{})
			var attempts atomic.Int32
			daemon := mcp.NewServer(&mcp.Implementation{Name: "initialize", Version: "1"}, nil)
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var request struct{ Method string }
					_ = json.Unmarshal(body, &request)
					if request.Method == "initialize" && attempts.Add(1) == 1 {
						close(started)
						<-r.Context().Done()
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			relay := &daemonSession{client: client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("session"))}
			defer func() { cancel(); relay.close() }()
			owner, stop := context.WithCancel(ctx)
			defer stop()
			expired := make(chan struct{})
			var expire sync.Once
			if deadline {
				owner = &initializationDeadlineContext{Context: context.Background(), expired: expired}
				defer expire.Do(func() { close(expired) })
			}
			ownerResult := make(chan error, 1)
			go func() { _, err := relay.get(owner, nil); ownerResult <- err }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			results := make(chan error, 3)
			for range 3 {
				waiter := &initializationWaiterContext{Context: ctx, joined: make(chan struct{})}
				go func() {
					session, err := relay.get(waiter, nil)
					if err == nil {
						err = session.Ping(waiter, nil)
					}
					results <- err
				}()
				select {
				case <-waiter.joined:
				case <-ctx.Done():
					t.Fatal("waiter did not join", ctx.Err())
				}
			}
			if deadline {
				expire.Do(func() { close(expired) })
			} else {
				stop()
			}
			select {
			case err := <-ownerResult:
				if !errors.Is(err, owner.Err()) {
					t.Fatalf("initiator error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for range 3 {
				select {
				case err := <-results:
					if err != nil {
						t.Fatal("live waiter inherited initiator failure", err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("wanted one retry shared by waiters, got %d initializations", got)
			}
		})
	}
}
