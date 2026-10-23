package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

func keyedPost(t *testing.T, h *senderHarness, principal, key, body string) (int, []byte) {
	t.Helper()
	r, _ := http.NewRequest("POST", h.srv.URL+"/messages", bytes.NewBufferString(body))
	r.Header.Set("X-Test-Principal", principal)
	r.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Errorf("post: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// The idempotency namespace is the verified principal, not the From a caller
// claims: a second principal claiming the same From cannot collide with, or learn
// about, the first one's key (CW-20261023-0018, Reviewer hold on #500).
func TestIdempotencyKeyNamespaceIsTheVerifiedPrincipal(t *testing.T) {
	h := newSenderHarness(t, identity.Observe)
	body := `{"from":"msg://session/local/s1","to":"msg://agent/test/target","kind":"notice","payload":{"text":"same"}}`
	code, first := keyedPost(t, h, "session", "private-key", body)
	if code != http.StatusCreated {
		t.Fatalf("first send: %d %s", code, first)
	}
	// The same principal repeating the key gets the stored message back.
	if code, again := keyedPost(t, h, "session", "private-key", body); code != http.StatusCreated || !bytes.Contains(again, firstID(first)) {
		t.Fatalf("same principal repeat: %d %s, want 201 with the first id", code, again)
	}
	// Another principal claiming the same From is in its own namespace: the used
	// key behaves like an unused one and never reveals the first message.
	code, raw := keyedPost(t, h, "agentsession", "private-key", body)
	fresh, _ := keyedPost(t, h, "agentsession", "unused-key", body)
	if code != fresh {
		t.Fatalf("another principal probes the key: used=%d fresh=%d body=%s", code, fresh, raw)
	}
	if bytes.Contains(raw, firstID(first)) {
		t.Fatalf("another principal's response names the first message: %s", raw)
	}
	var n int
	if err := h.db.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE to_urn = ?`, "msg://agent/test/target").Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows = %d err=%v, want 3 (first, other principal's two)", n, err)
	}
}

func firstID(created []byte) []byte {
	const k = `"id":"`
	i := bytes.Index(created, []byte(k))
	if i < 0 {
		return []byte("no-id-found")
	}
	rest := created[i+len(k):]
	return rest[:bytes.IndexByte(rest, '"')]
}

// A shared or delegated sender address does not share keys: the namespace is the
// verified principal, so a principal that may act as the address and another that
// merely claims it each have their own.
func TestIdempotencyKeyIsNotSharedThroughADelegatedAddress(t *testing.T) {
	h := newSenderHarness(t, identity.Observe)
	body := `{"from":"msg://agent/agent-mux/builder","to":"msg://agent/test/delegated-target","kind":"notice","payload":{"text":"same"}}`
	code, first := keyedPost(t, h, "agentsession", "shared-key", body) // may act as builder
	if code != http.StatusCreated {
		t.Fatalf("delegate send: %d %s", code, first)
	}
	used, raw := keyedPost(t, h, "sendas", "shared-key", body) // only claims builder
	fresh, _ := keyedPost(t, h, "sendas", "other-key", body)
	if used != fresh || bytes.Contains(raw, firstID(first)) {
		t.Fatalf("a claimant of the shared address reached the delegate's key: used=%d fresh=%d body=%s", used, fresh, raw)
	}
	// The delegate itself still dedupes.
	if code, again := keyedPost(t, h, "agentsession", "shared-key", body); code != http.StatusCreated || !bytes.Contains(again, firstID(first)) {
		t.Fatalf("delegate repeat: %d %s", code, again)
	}
}

// A send whose enqueue committed but whose content insert failed is repaired by a
// retry with the same key: one content row, no duplicate (the half-commit path).
func TestRetryRepairsEnqueueThenInsertFailure(t *testing.T) {
	h := newSenderHarness(t, identity.Enforce)
	if _, err := h.db.DB().Exec(`CREATE TRIGGER fail_insert BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL,'insertion fault'); END;`); err != nil {
		t.Fatal(err)
	}
	body := `{"to":"msg://agent/test/target","kind":"notice","payload":{"text":"same"}}`
	if code, _ := keyedPost(t, h, "session", "repair-key", body); code != http.StatusInternalServerError {
		t.Fatalf("fault status %d, want 500", code)
	}
	if _, err := h.db.DB().Exec(`DROP TRIGGER fail_insert`); err != nil {
		t.Fatal(err)
	}
	if code, raw := keyedPost(t, h, "session", "repair-key", body); code != http.StatusCreated {
		t.Fatalf("retry %d %s", code, raw)
	}
	var n int
	if err := h.db.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("content rows = %d err=%v, want 1", n, err)
	}
}

func metadataPost(t *testing.T, h *senderHarness, principal, path, body string) (int, []byte) {
	t.Helper()
	r, _ := http.NewRequest("POST", h.srv.URL+path, bytes.NewBufferString(body))
	r.Header.Set("X-Test-Principal", principal)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Errorf("post: %v", err)
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// A key set only as tether.message_id metadata, with no header, is scoped to the
// verified principal the same way, on both send and notify.
func TestMetadataOnlyIdempotencyKeyIsScopedToTheVerifiedPrincipal(t *testing.T) {
	for _, path := range []string{"/messages", "/messages/notify"} {
		t.Run(path, func(t *testing.T) {
			h := newSenderHarness(t, identity.Observe)
			mk := func(key string) string {
				return `{"from":"msg://session/local/s1","to":"msg://agent/test/meta-target","kind":"notice","payload":{"text":"same"},"metadata":{"tether.message_id":"` + key + `"}}`
			}
			code, first := metadataPost(t, h, "session", path, mk("meta-key"))
			if code != http.StatusCreated {
				t.Fatalf("first send: %d %s", code, first)
			}
			// The same principal repeating it dedupes.
			if code, again := metadataPost(t, h, "session", path, mk("meta-key")); code != http.StatusCreated || !bytes.Contains(again, firstID(first)) {
				t.Fatalf("same principal repeat: %d %s", code, again)
			}
			// Another principal claiming the same From behaves as with an unused key.
			used, raw := metadataPost(t, h, "agentsession", path, mk("meta-key"))
			fresh, _ := metadataPost(t, h, "agentsession", path, mk("unused-meta-key"))
			if used != fresh || bytes.Contains(raw, firstID(first)) {
				t.Fatalf("metadata key probed across principals: used=%d fresh=%d body=%s", used, fresh, raw)
			}
		})
	}
}

// POST /messages/request takes the same client key and scopes it the same way.
func TestRequestRouteScopesAMetadataKeyToTheVerifiedPrincipal(t *testing.T) {
	h := newSenderHarness(t, identity.Observe)
	body := `{"from":"msg://session/local/s1","to":"msg://agent/test/request-target","kind":"request","payload":{"text":"q"},"metadata":{"tether.message_id":"req-key"}}`
	// The request waits for a reply that never comes; it is stored as soon as it is
	// accepted. Wait for the row rather than assuming the timeout left one.
	go metadataPost(t, h, "session", "/messages/request?timeout=1500ms", body)
	var meta string
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := h.db.DB().QueryRow(`SELECT metadata FROM messages WHERE to_urn = ?`, "msg://agent/test/request-target").Scan(&meta)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the request was never stored: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(meta, `"tether.message_id":"p`) || strings.Contains(meta, `"tether.message_id":"req-key"`) {
		t.Fatalf("stored metadata = %s, want the key scoped to the principal", meta)
	}
}

// Concurrent sends with one key converge: every request answers 201 and exactly one
// message is stored, whichever of them won the enqueue and whichever inserted the row.
func TestConcurrentKeyedSendsConverge(t *testing.T) {
	h := newSenderHarness(t, identity.Observe)
	const n, rounds = 20, 8
	for round := 0; round < rounds; round++ {
		to := fmt.Sprintf("msg://agent/test/concurrent-target-%d", round)
		body := `{"kind":"notice","from":"msg://session/local/s1","to":"` + to + `","payload":{"text":"same"}}`
		key := fmt.Sprintf("concurrent-key-%d", round)
		codes := make(chan int, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, _ := keyedPost(t, h, "session", key, body)
				codes <- code
			}()
		}
		wg.Wait()
		close(codes)
		for code := range codes {
			if code != http.StatusCreated {
				t.Errorf("round %d: concurrent keyed send status = %d, want 201", round, code)
			}
		}
		var rows int
		if err := h.db.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE to_urn = ?`, to).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("round %d: rows = %d err=%v, want 1", round, rows, err)
		}
	}
}
