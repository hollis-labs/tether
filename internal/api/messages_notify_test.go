package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func TestMessageNotify_SendsAndWakesExplicitSession(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc := &fakeLaunchService{
		getRes: map[string]*store.SessionRow{
			"s1": {ID: "s1", State: string(session.StateRunning), LogicalAgentID: "worker"},
		},
		runtimeHealthOK: map[string]bool{"s1": true},
	}
	h := NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore()})

	body := []byte(`{
		"from":"msg://user/local/operator",
		"to":"msg://agent/local/worker",
		"kind":"notice",
		"urgency":"high",
		"session_id":"s1",
		"payload":{"subject":"Wake smoke","body":"check inbox"}
	}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/notify", bytes.NewReader(body))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out messageNotifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Message.ID == "" || out.UnreadCount != 1 {
		t.Fatalf("notify response = %+v", out)
	}
	if !out.WakeAttempted || !out.WakeDelivered || out.SessionID != "s1" {
		t.Fatalf("wake fields = %+v", out)
	}
	if got := string(svc.inputLog[0]); !strings.Contains(got, "Mailbox wake") || !strings.Contains(got, out.Message.ID) || !strings.Contains(got, "urgency `high`") {
		t.Fatalf("wake text = %q", got)
	}
	got, err := db.MessagingStore().Get(req.Context(), out.Message.ID)
	if err != nil {
		t.Fatalf("get sent message: %v", err)
	}
	if got.Metadata["urgency"] != "high" {
		t.Fatalf("urgency metadata = %q", got.Metadata["urgency"])
	}
}

// TestMessageNotify_RejectsUnrelatedSessionOverride is T05's (messaging
// vNext) fix for a real spoofing gap: resolveNotifySession's explicit
// session_id override previously only checked the session existed and was
// running -- never that it had anything to do with the message's actual
// recipient. A caller could address a message "to" one logical agent while
// specifying an unrelated session_id and wake THAT session instead,
// injecting a fabricated "you have unread messages" turn into a session
// that was never the message's recipient. Sending a message and waking an
// arbitrary session are different capabilities (architecture:
// "Publication/wake/admin capabilities differ from permission to send").
func TestMessageNotify_RejectsUnrelatedSessionOverride(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify-unrelated.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// s2 belongs to a DIFFERENT logical agent than the message's recipient
	// (worker) -- s2 is the "unrelated session" a spoofed override would
	// try to wake.
	svc := &fakeLaunchService{
		getRes: map[string]*store.SessionRow{
			"s2": {ID: "s2", State: string(session.StateRunning), LogicalAgentID: "unrelated-agent"},
		},
		runtimeHealthOK: map[string]bool{"s2": true},
	}
	h := NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore()})

	body := []byte(`{
		"from":"msg://user/local/operator",
		"to":"msg://agent/local/worker",
		"kind":"notice",
		"session_id":"s2",
		"payload":{"body":"spoofed wake attempt"}
	}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/notify", bytes.NewReader(body))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out messageNotifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The message itself is still stored (sending always succeeds) --
	// only the wake is rejected.
	if out.Message.ID == "" {
		t.Fatalf("expected the message to still be stored, got %+v", out)
	}
	if out.WakeDelivered {
		t.Fatalf("expected the unrelated session override to be rejected, but wake was delivered: %+v", out)
	}
	if out.WakeError == "" {
		t.Fatalf("expected a wake_error explaining the rejected override, got %+v", out)
	}
	if len(svc.inputLog) != 0 {
		t.Fatalf("expected NO SendTurn call against the unrelated session, got %d calls: %q", len(svc.inputLog), svc.inputLog)
	}
}

// TestMessageNotify_ExplicitSessionIDMustMatchSessionKindRecipient covers
// the msg://session/... recipient-kind half of the same fix: the override
// must equal the recipient's own session ID, not merely exist and be
// running.
func TestMessageNotify_ExplicitSessionIDMustMatchSessionKindRecipient(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify-session-kind.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc := &fakeLaunchService{
		getRes: map[string]*store.SessionRow{
			"other-session": {ID: "other-session", State: string(session.StateRunning), LogicalAgentID: "someone"},
		},
		runtimeHealthOK: map[string]bool{"other-session": true, "intended-session": true},
	}
	h := NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore()})

	body := []byte(`{
		"from":"msg://user/local/operator",
		"to":"msg://session/local/intended-session",
		"kind":"notice",
		"session_id":"other-session",
		"payload":{"body":"spoofed session-kind override"}
	}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/notify", bytes.NewReader(body))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out messageNotifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.WakeDelivered {
		t.Fatalf("expected the mismatched session-kind override to be rejected: %+v", out)
	}
	if len(svc.inputLog) != 0 {
		t.Fatalf("expected NO SendTurn call against the mismatched session, got %d calls", len(svc.inputLog))
	}
}

func TestMessageNotify_StoresWhenNoLiveSession(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify-no-session.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc := &fakeLaunchService{}
	h := NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore()})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/messages/notify", strings.NewReader(`{
		"from":"msg://user/local/operator",
		"to":"msg://agent/local/offline",
		"payload":{"body":"later"}
	}`))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out messageNotifyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Message.ID == "" || out.WakeAttempted || out.WakeDelivered || out.WakeError != "" {
		t.Fatalf("notify response = %+v", out)
	}
	if len(svc.inputLog) != 0 {
		t.Fatalf("unexpected wake input: %q", svc.inputLog)
	}
}
