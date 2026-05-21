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
