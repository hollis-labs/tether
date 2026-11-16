package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/terminalattach"
)

type terminalTestHost struct {
	mu      sync.Mutex
	binding terminalattach.Binding
	roles   map[string]terminalattach.Role
	lease   terminalattach.Lease
	seq     int
	input   [][]byte
}

func (h *terminalTestHost) Resolve(_ context.Context, target string) (terminalattach.Binding, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if target != h.binding.SessionID && target != h.binding.ConversationID {
		return terminalattach.Binding{}, terminalattach.ErrNotFound
	}
	return h.binding, nil
}
func (h *terminalTestHost) Attach(_ context.Context, b terminalattach.Binding, role terminalattach.Role) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b != h.binding {
		return "", terminalattach.ErrStaleGeneration
	}
	h.seq++
	id := "view-" + strconv.Itoa(h.seq)
	h.roles[id] = role
	return id, nil
}
func (h *terminalTestHost) Detach(_ context.Context, b terminalattach.Binding, id string, _ terminalattach.Role) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b != h.binding {
		return terminalattach.ErrStaleGeneration
	}
	delete(h.roles, id)
	if h.lease.WriterID == id {
		h.lease = terminalattach.Lease{}
	}
	return nil
}
func (h *terminalTestHost) Claim(_ context.Context, b terminalattach.Binding, id string, takeover bool) (terminalattach.Lease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b != h.binding {
		return terminalattach.Lease{}, terminalattach.ErrStaleGeneration
	}
	if h.roles[id] != terminalattach.ControllerRole {
		return terminalattach.Lease{}, terminalattach.ErrNotWriter
	}
	if h.lease.ID != "" && !takeover {
		return terminalattach.Lease{}, terminalattach.ErrWriterHeld
	}
	h.seq++
	h.lease = terminalattach.Lease{ID: "lease-" + strconv.Itoa(h.seq), Binding: b, WriterID: id, ExpiresAt: time.Now().Add(time.Minute)}
	return h.lease, nil
}
func (h *terminalTestHost) Renew(_ context.Context, l terminalattach.Lease) (terminalattach.Lease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l != h.lease {
		return terminalattach.Lease{}, terminalattach.ErrNotWriter
	}
	h.lease.ExpiresAt = time.Now().Add(time.Minute)
	return h.lease, nil
}
func (h *terminalTestHost) Observe(_ context.Context, b terminalattach.Binding, w io.Writer, _ int64, on func(terminalattach.Window) error) error {
	h.mu.Lock()
	current := h.binding
	h.mu.Unlock()
	if b != current {
		return terminalattach.ErrStaleGeneration
	}
	if err := on(terminalattach.Window{OldestOffset: 4, NextOffset: 8}); err != nil {
		return err
	}
	_, err := w.Write([]byte("pty"))
	return err
}
func (h *terminalTestHost) Input(_ context.Context, l terminalattach.Lease, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l.Binding != h.binding {
		return terminalattach.ErrStaleGeneration
	}
	if l.ID == "" || l != h.lease {
		return terminalattach.ErrNotWriter
	}
	h.input = append(h.input, bytes.Clone(data))
	return nil
}
func (h *terminalTestHost) Resize(_ context.Context, l terminalattach.Lease, _, _ uint16) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l.Binding != h.binding {
		return terminalattach.ErrStaleGeneration
	}
	if l.ID == "" || l != h.lease {
		return terminalattach.ErrNotWriter
	}
	return nil
}
func terminalRequest(h http.Handler, method, path, body, lease string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if lease != "" {
		req.Header.Set("X-Tether-Writer-Lease", lease)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestTerminalAttachmentsHTTP(t *testing.T) {
	backend := &terminalTestHost{binding: terminalattach.Binding{OwnerID: "owner", OperativeID: "agent", ConversationID: "conv", AttemptID: "attempt-1", SessionID: "live-1", Generation: 3, Terminal: true}, roles: map[string]terminalattach.Role{}}
	h := NewHandler(Deps{TerminalHost: backend})
	if rr := terminalRequest(h, "POST", "/terminal-attachments", `{"target":"live-1","role":"admin"}`, ""); rr.Code != 400 {
		t.Fatalf("invalid role: %d", rr.Code)
	}
	open := func(role string) terminalattach.Attachment {
		body := `{"target":"live-1","role":"` + role + `"}`
		rr := terminalRequest(h, "POST", "/terminal-attachments", body, "")
		if rr.Code != 201 {
			t.Fatalf("open: %d %s", rr.Code, rr.Body.String())
		}
		var a terminalattach.Attachment
		if err := json.Unmarshal(rr.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	viewer, controller, other := open("viewer"), open("controller"), open("controller")
	if rr := terminalRequest(h, "POST", "/terminal-attachments/"+viewer.ID+"/lease", `{}`, ""); rr.Code != 403 {
		t.Fatalf("viewer claim: %d", rr.Code)
	}
	if rr := terminalRequest(h, "POST", "/terminal-attachments/"+viewer.ID+"/input", "observer", ""); rr.Code != 403 {
		t.Fatalf("viewer input: %d", rr.Code)
	}
	if rr := terminalRequest(h, "POST", "/terminal-attachments/"+viewer.ID+"/resize", `{"rows":24,"cols":80}`, ""); rr.Code != 403 {
		t.Fatalf("viewer resize: %d", rr.Code)
	}
	stream := terminalRequest(h, "GET", "/terminal-attachments/"+viewer.ID+"/stream?since_offset=1", "", "")
	if stream.Code != 200 || stream.Body.String() != "pty" || stream.Header().Get("X-Tether-Attach-Evicted") != "true" || stream.Header().Get("X-Tether-Terminal-Diagnostic") != "true" {
		t.Fatalf("stream: %d %q %+v", stream.Code, stream.Body.String(), stream.Header())
	}
	claim := terminalRequest(h, "POST", "/terminal-attachments/"+controller.ID+"/lease", `{}`, "")
	if claim.Code != 200 {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body.String())
	}
	var first struct {
		Lease string `json:"lease"`
	}
	if err := json.Unmarshal(claim.Body.Bytes(), &first); err != nil || first.Lease == "" {
		t.Fatalf("lease: %v %q", err, claim.Body.String())
	}
	if rr := terminalRequest(h, "POST", "/terminal-attachments/"+other.ID+"/lease", `{}`, ""); rr.Code != 409 {
		t.Fatalf("competing writer: %d", rr.Code)
	}
	if rr := terminalRequest(h, "POST", "/terminal-attachments/"+controller.ID+"/input", "accepted", first.Lease); rr.Code != 204 {
		t.Fatalf("writer input: %d %s", rr.Code, rr.Body.String())
	}
	claim = terminalRequest(h, "POST", "/terminal-attachments/"+other.ID+"/lease", `{"takeover":true}`, "")
	if claim.Code != 200 {
		t.Fatalf("takeover: %d %s", claim.Code, claim.Body.String())
	}
	if rr := terminalRequest(h, "POST", "/terminal-attachments/"+controller.ID+"/input", "stale", first.Lease); rr.Code != 403 {
		t.Fatalf("revoked writer: %d", rr.Code)
	}
	backend.mu.Lock()
	backend.binding.SessionID = "live-2"
	backend.binding.AttemptID = "attempt-2"
	backend.binding.Generation = 4
	backend.mu.Unlock()
	if rr := terminalRequest(h, "POST", "/terminal-attachments/"+other.ID+"/input", "old", first.Lease); rr.Code != 409 {
		t.Fatalf("old generation: %d", rr.Code)
	}
	if rr := terminalRequest(h, "POST", "/terminal-attachments", `{"target":"live-1"}`, ""); rr.Code != 404 {
		t.Fatalf("old direct target: %d", rr.Code)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.input) != 1 || string(backend.input[0]) != "accepted" {
		t.Fatalf("host input: %q", backend.input)
	}
}

// This test pins the pre-activation boundary. CW-20261116-0019 deliberately
// changes the direct routes once the daemon has an authoritative PTY mapping.
func TestTerminalAttachmentOptInAndLegacyBypass(t *testing.T) {
	service := &fakeLaunchService{}
	off := NewHandler(Deps{Service: service})
	if rr := terminalRequest(off, http.MethodPost, "/terminal-attachments", `{"target":"live-1"}`, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("opt-in off: %d", rr.Code)
	}
	for _, route := range []struct{ path, body string }{
		{"/sessions/live-1/input", "legacy off"},
		{"/sessions/live-1/resize", `{"rows":24,"cols":80}`},
	} {
		if rr := terminalRequest(off, http.MethodPost, route.path, route.body, ""); rr.Code != http.StatusNoContent {
			t.Fatalf("legacy off %s: %d %s", route.path, rr.Code, rr.Body.String())
		}
	}

	backend := &terminalTestHost{binding: terminalattach.Binding{OwnerID: "owner", OperativeID: "agent", ConversationID: "conv", AttemptID: "attempt", SessionID: "live-1", Generation: 1, Terminal: true}, roles: map[string]terminalattach.Role{}}
	on := NewHandler(Deps{Service: service, TerminalHost: backend})
	opened := terminalRequest(on, http.MethodPost, "/terminal-attachments", `{"target":"live-1","role":"controller"}`, "")
	if opened.Code != http.StatusCreated {
		t.Fatalf("open: %d %s", opened.Code, opened.Body.String())
	}
	var attachment terminalattach.Attachment
	if err := json.Unmarshal(opened.Body.Bytes(), &attachment); err != nil {
		t.Fatal(err)
	}
	if rr := terminalRequest(on, http.MethodPost, "/terminal-attachments/"+attachment.ID+"/lease", `{}`, ""); rr.Code != http.StatusOK {
		t.Fatalf("writer claim: %d %s", rr.Code, rr.Body.String())
	}
	for _, route := range []struct{ path, body string }{
		{"/sessions/live-1/input", "legacy bypass"},
		{"/sessions/live-1/resize", `{"rows":40,"cols":100}`},
	} {
		if rr := terminalRequest(on, http.MethodPost, route.path, route.body, ""); rr.Code != http.StatusNoContent {
			t.Fatalf("legacy bypass %s: %d %s", route.path, rr.Code, rr.Body.String())
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.inputLog) != 2 || string(service.inputLog[0]) != "legacy off" || string(service.inputLog[1]) != "legacy bypass" || len(service.resizeRows) != 2 || service.resizeRows[0] != 24 || service.resizeRows[1] != 40 {
		t.Fatalf("legacy effects: input=%q resize=%v", service.inputLog, service.resizeRows)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.input) != 0 || backend.lease.ID == "" {
		t.Fatalf("legacy call unexpectedly consumed lease: input=%q lease=%+v", backend.input, backend.lease)
	}
}
