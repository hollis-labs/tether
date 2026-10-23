package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261023-0018: a send repeated with the same Idempotency-Key returns the
// stored message; a client that timed out and retried must not make a second one.
func TestSendIdempotencyKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "idem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore()}))
	t.Cleanup(srv.Close)

	body := func(from, to, text string) []byte {
		b, _ := json.Marshal(map[string]any{
			"kind": "notice", "from": from, "to": to,
			"payload": map[string]string{"text": text}, "content_type": "application/json",
		})
		return b
	}
	post := func(path, key string, b []byte) (int, messaging.Envelope) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var env messaging.Envelope
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return resp.StatusCode, env
	}
	rows := func(to string) int {
		t.Helper()
		var n int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE to_urn = ?`, to).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	from := "msg://agent/test/sender"
	to := "msg://agent/test/idem-recipient"

	code, first := post("/messages", "key-1", body(from, to, "hello"))
	if code != http.StatusCreated || first.ID == "" {
		t.Fatalf("first send: %d %+v", code, first)
	}
	code, again := post("/messages", "key-1", body(from, to, "hello"))
	if code != http.StatusCreated || again.ID != first.ID {
		t.Fatalf("repeat with the same key: %d id=%q, want 201 and %q", code, again.ID, first.ID)
	}
	if n := rows(to); n != 1 {
		t.Fatalf("%d message rows after a repeated send, want 1", n)
	}

	// No key: two identical sends are two messages, as before.
	to2 := "msg://agent/test/idem-recipient-2"
	_, a := post("/messages", "", body(from, to2, "same"))
	_, b := post("/messages", "", body(from, to2, "same"))
	if a.ID == "" || a.ID == b.ID || rows(to2) != 2 {
		t.Fatalf("unkeyed sends: %q %q rows=%d, want two messages", a.ID, b.ID, rows(to2))
	}

	// The same key with a different body is a conflict, not a silent overwrite.
	if code, _ := post("/messages", "key-1", body(from, to, "different")); code != http.StatusConflict {
		t.Fatalf("same key, new body: %d, want 409", code)
	}
	// The key is scoped by sender.
	_, other := post("/messages", "key-1", body("msg://agent/test/other-sender", to, "hello"))
	if other.ID == "" || other.ID == first.ID {
		t.Fatalf("another sender's message with the same key: %q, want a new message", other.ID)
	}
	// An oversized key is refused.
	if code, _ := post("/messages", strings.Repeat("k", maxIdempotencyKeyBytes+1), body(from, to, "x")); code != http.StatusBadRequest {
		t.Fatalf("oversized key: %d, want 400", code)
	}
	// A key set both ways must agree.
	b2, _ := json.Marshal(map[string]any{"kind": "notice", "from": from, "to": to, "metadata": map[string]string{"tether.message_id": "other"}})
	if code, _ := post("/messages", "key-2", b2); code != http.StatusBadRequest {
		t.Fatalf("header and metadata disagree: %d, want 400", code)
	}
}
