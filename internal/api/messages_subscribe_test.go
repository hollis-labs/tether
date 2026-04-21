package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/go-messaging"

	"github.com/chrispian/agent-mux/internal/store"
)

func newMessageTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "msg_sub.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ms := db.MessagingStore()
	h := NewHandler(Deps{MessageStore: ms})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, db
}

func TestHandleMessagesSubscribe_SSEFraming(t *testing.T) {
	srv, db := newMessageTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/messages/subscribe?to="+alice.URN(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Give the handler time to register its subscription.
	time.Sleep(50 * time.Millisecond)

	// Send a message through the store.
	bob := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "bob"}
	ms := db.MessagingStore()
	sent, err := ms.Send(context.Background(), messaging.Envelope{
		Kind:    messaging.MsgKindNotice,
		From:    bob,
		To:      alice,
		Payload: json.RawMessage(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Read SSE events until we see the message or timeout.
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var env messaging.Envelope
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			continue
		}
		if env.ID == sent.ID {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("did not receive sent envelope %s over SSE within 3s", sent.ID)
	}
}

func TestHandleMessagesSubscribe_MethodNotAllowed(t *testing.T) {
	srv, _ := newMessageTestServer(t)
	resp, err := http.Post(srv.URL+"/messages/subscribe", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestHandleMessagesSubscribe_MissingTo(t *testing.T) {
	srv, _ := newMessageTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/messages/subscribe", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
