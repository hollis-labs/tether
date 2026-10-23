package api

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func keyedPost(t *testing.T, h *senderHarness, principal, key, body string) (int, []byte) {
	t.Helper()
	r, _ := http.NewRequest("POST", h.srv.URL+"/messages", bytes.NewBufferString(body))
	r.Header.Set("X-Test-Principal", principal)
	r.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
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
