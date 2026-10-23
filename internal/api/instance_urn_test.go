package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/store"
)

// instanceFake adds the optional instance resolver to the API test fake.
type instanceFake struct {
	*fakeLaunchService
	live  map[string]string // session id -> logical agent
	ended map[string]string // ended session id -> successor ("" none)
}

func (f *instanceFake) ResolveInstanceSession(_ context.Context, to messaging.Address) (string, error) {
	if agent, ok := f.live[to.SubID]; ok && agent == to.ID {
		return to.SubID, nil
	}
	if succ, ok := f.ended[to.SubID]; ok {
		return "", &InstanceEndedError{SessionID: to.SubID, SupersededBy: succ}
	}
	return "", ErrInstanceUnknown
}

// CW-20261023-0018: a send to an agent instance address is checked before anything is
// stored, and notify wakes exactly that session.
func TestSendToAnInstanceAddress(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "inst.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := &fakeLaunchService{
		getRes:          map[string]*store.SessionRow{"s-live": {ID: "s-live", LogicalAgentID: "general"}},
		runtimeHealthOK: map[string]bool{"s-live": true},
	}
	svc := &instanceFake{fakeLaunchService: base, live: map[string]string{"s-live": "general"}, ended: map[string]string{"s-old": "s-new", "s-orphan": ""}}
	srv := httptest.NewServer(NewHandler(Deps{Service: svc, MessageStore: db.MessagingStore()}))
	t.Cleanup(srv.Close)

	post := func(path, to string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{"kind": "notice", "from": "msg://agent/test/sender", "to": to, "payload": map[string]string{"text": "hi"}})
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	rows := func(to string) int {
		var n int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE to_urn = ?`, to).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	live := "msg://agent/agent-mux/general/s-live"
	if code, _ := post("/messages", live); code != http.StatusCreated || rows(live) != 1 {
		t.Fatalf("send to a running instance: %d, rows %d", code, rows(live))
	}
	// Notify wakes exactly that session, and the receipt names it.
	code, out := post("/messages/notify", live)
	if code != http.StatusCreated || out["session_id"] != "s-live" {
		t.Fatalf("notify to a running instance: %d %v, want 201 session_id s-live", code, out)
	}
	if len(base.attemptWakeLog) != 1 || base.attemptWakeLog[0].SessionID != "s-live" {
		t.Fatalf("wake attempts = %+v, want one for s-live", base.attemptWakeLog)
	}
	// An ended instance: 409 naming the successor, nothing stored, no wake.
	for _, path := range []string{"/messages", "/messages/notify"} {
		ended := "msg://agent/agent-mux/general/s-old"
		code, out = post(path, ended)
		errObj, _ := out["error"].(map[string]any)
		details, _ := errObj["details"].(map[string]any)
		if code != http.StatusConflict || errObj["code"] != "instance_ended" || details["superseded_by"] != "s-new" || rows(ended) != 0 {
			t.Fatalf("%s to an ended instance: %d %v rows %d, want 409 instance_ended superseded_by s-new, nothing stored", path, code, out, rows(ended))
		}
	}
	// Ended with no known successor: still 409, no superseded_by.
	code, out = post("/messages", "msg://agent/agent-mux/general/s-orphan")
	if errObj, _ := out["error"].(map[string]any); code != http.StatusConflict || errObj["details"].(map[string]any)["superseded_by"] != nil {
		t.Fatalf("ended without successor: %d %v", code, out)
	}
	// Unknown, or a live session of another agent: 404, not told apart.
	for _, to := range []string{"msg://agent/agent-mux/general/nope", "msg://agent/agent-mux/other/s-live"} {
		if code, out = post("/messages", to); code != http.StatusNotFound || rows(to) != 0 {
			t.Fatalf("%s: %d %v rows %d, want 404 and nothing stored", to, code, out, rows(to))
		}
	}
	// A bare agent address is not an instance address and is unaffected.
	if code, _ := post("/messages", "msg://agent/agent-mux/general"); code != http.StatusCreated {
		t.Fatalf("bare agent address: %d", code)
	}
}
