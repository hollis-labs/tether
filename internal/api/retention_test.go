package api

// retention_test.go — end-to-end HTTP coverage for T09 acceptance #3's
// retention half: GET /messages/retention/candidates and POST
// /messages/{id}/purge. Store-layer eligibility rules (what counts as
// "pending obligation") are exercised thoroughly in
// internal/store/retention_test.go; this file only proves the HTTP
// plumbing over that policy -- status codes, request validation, and the
// not-configured 404.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/store"
)

func newRetentionTestServer(t *testing.T) (string, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "retention-api.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{MessageStore: db.MessagingStore(), Retention: db}))
	t.Cleanup(srv.Close)
	return srv.URL, db
}

func postPurge(t *testing.T, base, messageID string, body map[string]any) (*http.Response, messagePurgeResponse) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	resp, err := http.Post(base+"/messages/"+messageID+"/purge", "application/json", reader)
	if err != nil {
		t.Fatalf("POST purge: %v", err)
	}
	defer resp.Body.Close()
	var out messagePurgeResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp, out
}

func sendAPIRetentionMessage(t *testing.T, db *store.Store) string {
	t.Helper()
	sent, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{
		Kind:    messaging.MsgKindNotice,
		From:    messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"},
		To:      messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"},
		Payload: json.RawMessage(`{"body":"hello"}`),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return sent.ID
}

// backdateMessage rewrites created_at directly -- Send always stamps
// time.Now(), so a message sent moments ago needs this to become old
// enough to appear as a retention candidate under a realistic
// older_than_hours cutoff.
func backdateMessage(t *testing.T, db *store.Store, id string, age time.Duration) {
	t.Helper()
	if _, err := db.DB().Exec(`UPDATE messages SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-age).Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("backdate message %s: %v", id, err)
	}
}

func TestMessageRetentionCandidates_ListsPendingAndDeliveredWithEligibility(t *testing.T) {
	base, db := newRetentionTestServer(t)

	pendingID := sendAPIRetentionMessage(t, db)
	backdateMessage(t, db, pendingID, 48*time.Hour)

	deliveredID := sendAPIRetentionMessage(t, db)
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	if err := db.MessagingStore().Consume(context.Background(), deliveredID, to); err != nil {
		t.Fatalf("consume: %v", err)
	}
	backdateMessage(t, db, deliveredID, 48*time.Hour)

	// A message sent moments ago (not backdated) must NOT appear under a
	// 1-hour cutoff -- proves older_than_hours is actually honored, not
	// just accepted and ignored.
	tooRecentID := sendAPIRetentionMessage(t, db)

	resp, err := http.Get(base + "/messages/retention/candidates?older_than_hours=1")
	if err != nil {
		t.Fatalf("GET candidates: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Candidates []retentionCandidateDTO `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]retentionCandidateDTO{}
	for _, c := range out.Candidates {
		byID[c.MessageID] = c
	}

	if _, present := byID[tooRecentID]; present {
		t.Errorf("candidate list includes %s under older_than_hours=1, but it was sent moments ago", tooRecentID)
	}
	if pending, ok := byID[pendingID]; !ok || pending.Eligible {
		t.Errorf("pending candidate = %+v (ok=%v), want present and Eligible=false", pending, ok)
	}
	if delivered, ok := byID[deliveredID]; !ok || !delivered.Eligible {
		t.Errorf("delivered candidate = %+v (ok=%v), want present and Eligible=true", delivered, ok)
	}
}

func TestMessageRetentionCandidates_InvalidOlderThanHours_400(t *testing.T) {
	base, _ := newRetentionTestServer(t)
	resp, err := http.Get(base + "/messages/retention/candidates?older_than_hours=notanumber")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMessagePurge_PendingDelivery_Conflict409(t *testing.T) {
	base, db := newRetentionTestServer(t)
	id := sendAPIRetentionMessage(t, db)

	resp, _ := postPurge(t, base, id, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (pending obligation)", resp.StatusCode)
	}

	env, err := db.MessagingStore().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get after refused purge: %v", err)
	}
	if string(env.Payload) != `{"body":"hello"}` {
		t.Errorf("payload after a refused (409) purge = %q, want unchanged", env.Payload)
	}
}

func TestMessagePurge_DeadLettered_Conflict409(t *testing.T) {
	base, db := newRetentionTestServer(t)
	id := sendAPIRetentionMessage(t, db)
	deadLetterDelivery(t, db, id, "s1")

	resp, _ := postPurge(t, base, id, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 -- a dead-lettered delivery remains redrivable, purging it first "+
			"would make a later redrive resend an empty message", resp.StatusCode)
	}
}

func TestMessagePurge_Delivered_HappyPathAndIdempotent(t *testing.T) {
	base, db := newRetentionTestServer(t)
	id := sendAPIRetentionMessage(t, db)
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	if err := db.MessagingStore().Consume(context.Background(), id, to); err != nil {
		t.Fatalf("consume: %v", err)
	}

	resp, out := postPurge(t, base, id, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusOK || !out.Purged {
		t.Fatalf("first purge: status=%d out=%+v, want 200/purged=true", resp.StatusCode, out)
	}

	resp2, out2 := postPurge(t, base, id, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second purge: status=%d, want 200 (idempotent, not an error)", resp2.StatusCode)
	}
	assertPurgeReceipt(t, db, id, "msg://agent/test/operator")
	if out2.Purged {
		t.Fatalf("second purge: %+v, want purged=false (already purged)", out2)
	}
}

func TestMessagePurge_RequiresAuthorizedBy(t *testing.T) {
	base, db := newRetentionTestServer(t)
	id := sendAPIRetentionMessage(t, db)
	resp, _ := postPurge(t, base, id, map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMessagePurge_UnknownMessage_404(t *testing.T) {
	base, _ := newRetentionTestServer(t)
	resp, _ := postPurge(t, base, "no-such-message", map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMessageRetention_NotConfigured_404(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "no-retention.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{MessageStore: db.MessagingStore()}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/messages/retention/candidates")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("candidates status = %d, want 404", resp.StatusCode)
	}

	id := sendAPIRetentionMessage(t, db)
	resp2, _ := postPurge(t, srv.URL, id, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("purge status = %d, want 404", resp2.StatusCode)
	}
}

// Verify the validated HTTP author reaches durable storage, not just a log.
func assertPurgeReceipt(t *testing.T, db *store.Store, id, author string) {
	t.Helper()
	var table, messageID, authorizedBy, at string
	if err := db.DB().QueryRow(`SELECT table_name,message_id,authorized_by,at FROM message_purge_audit WHERE message_id=?`, id).Scan(&table, &messageID, &authorizedBy, &at); err != nil {
		t.Fatal(err)
	}
	if table != "messages" || messageID != id || authorizedBy != author {
		t.Fatalf("receipt: %s %s %s", table, messageID, authorizedBy)
	}
	if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
		t.Fatalf("receipt time: %v", err)
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM message_purge_audit WHERE message_id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate/missing receipt: %d, %v", count, err)
	}
}

func TestMessagePurge_AuditFailureReturns500AndKeepsBody(t *testing.T) {
	base, db := newRetentionTestServer(t)
	id := sendAPIRetentionMessage(t, db)
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	if err := db.MessagingStore().Consume(context.Background(), id, to); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_message_purge_audit BEFORE INSERT ON message_purge_audit BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	resp, _ := postPurge(t, base, id, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", resp.StatusCode)
	}
	env, err := db.MessagingStore().Get(context.Background(), id)
	if err != nil || string(env.Payload) != `{"body":"hello"}` {
		t.Fatalf("failed audit lost body: %+v, %v", env, err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_message_purge_audit`); err != nil {
		t.Fatal(err)
	}
	resp, out := postPurge(t, base, id, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusOK || !out.Purged {
		t.Fatalf("retry: %d %+v", resp.StatusCode, out)
	}
	assertPurgeReceipt(t, db, id, "msg://agent/test/operator")
}

func TestMessagePurge_RefusedRequestsLeaveNoAudit(t *testing.T) {
	base, db := newRetentionTestServer(t)
	id := sendAPIRetentionMessage(t, db)
	for _, tc := range []struct {
		id, author string
		status     int
	}{
		{id, "", http.StatusBadRequest}, {id, "not-a-urn", http.StatusBadRequest},
		{id, "msg://agent/test/operator", http.StatusConflict},
		{"missing", "msg://agent/test/operator", http.StatusNotFound},
	} {
		resp, _ := postPurge(t, base, tc.id, map[string]any{"authorized_by": tc.author})
		if resp.StatusCode != tc.status {
			t.Fatalf("%s %q status %d want %d", tc.id, tc.author, resp.StatusCode, tc.status)
		}
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM message_purge_audit`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("refusal wrote receipt: %d, %v", count, err)
	}
}
