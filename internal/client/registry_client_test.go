package client

// registry_client_test.go — wire-format compliance tests for the typed
// RegistryClient. Coverage per the sprint spec:
//
//   - happy path for each op (Register, Lookup, Search, UpdateSelf,
//     Deregister, Sync)
//   - 404 → errors.Is(err, registry.ErrNotFound)
//   - 500 → wrapped error with body content surfaced
//   - unreachable → ErrDaemonUnreachable for the first call after the
//     listener closes
//   - Sync 204 returns (zero, false, nil); Sync 200 returns (profile,
//     true, nil)
//
// We stand up httptest.NewServer with canned responses rather than a
// real registry.Service — these tests are about the client's wire
// behavior, not about service correctness (the latter is exercised by
// internal/api/registry_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

// newRegistryClient stands up a httptest.Server with the given handler
// and returns a Client pointed at it. The server is t.Cleanup'd.
func newRegistryClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, http: srv.Client()}
}

// ─── Register ──────────────────────────────────────────────────────────────

func TestRegistryClient_Register_Happy(t *testing.T) {
	want := registry.Profile{
		URN:         "msg://agent/agent-mux/agt_alpha00001",
		Kind:        registry.KindAgent,
		DisplayName: "Alpha",
		Status:      registry.StatusActive,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/registry/agents" {
			t.Errorf("path = %q, want /registry/agents", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(want)
	})
	c := newRegistryClient(t, h)

	got, err := c.Registry().Register(context.Background(), registry.KindAgent, registry.Profile{
		DisplayName: "Alpha",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got.URN != want.URN {
		t.Errorf("URN = %q, want %q", got.URN, want.URN)
	}
}

func TestRegistryClient_Register_Error500(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
	})
	c := newRegistryClient(t, h)
	_, err := c.Registry().Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: "X"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error message missing body: %v", err)
	}
}

// ─── Lookup ───────────────────────────────────────────────────────────────

func TestRegistryClient_Lookup_Happy(t *testing.T) {
	want := registry.Profile{
		URN:         "msg://agent/agent-mux/agt_lookup0001",
		Kind:        registry.KindAgent,
		DisplayName: "Looked Up",
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		// Path must include the URL-escaped URN.
		if !strings.Contains(r.URL.EscapedPath(), "agt_lookup0001") {
			t.Errorf("path missing URN: %s", r.URL.EscapedPath())
		}
		_ = json.NewEncoder(w).Encode(want)
	})
	c := newRegistryClient(t, h)
	got, err := c.Registry().Lookup(context.Background(), want.URN)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.URN != want.URN {
		t.Errorf("URN = %q, want %q", got.URN, want.URN)
	}
}

func TestRegistryClient_Lookup_NotFound(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no row"}}`))
	})
	c := newRegistryClient(t, h)
	_, err := c.Registry().Lookup(context.Background(), "msg://agent/agent-mux/agt_missing0000")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("not wrapping ErrNotFound: %v", err)
	}
}

func TestRegistryClient_Lookup_UnknownURNPrefix(t *testing.T) {
	// Without a URN prefix the client can't infer the kind segment, so
	// it surfaces a local error before issuing the HTTP request.
	c := &Client{baseURL: "http://unused", http: http.DefaultClient}
	_, err := c.Registry().Lookup(context.Background(), "msg://something/else")
	if err == nil {
		t.Fatal("expected local error for unknown URN prefix")
	}
}

// ─── Search ───────────────────────────────────────────────────────────────

func TestRegistryClient_Search_Happy(t *testing.T) {
	page := []registry.Profile{
		{URN: "msg://agent/agent-mux/agt_a", Kind: registry.KindAgent, DisplayName: "A"},
		{URN: "msg://agent/agent-mux/agt_b", Kind: registry.KindAgent, DisplayName: "B"},
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/registry/agents" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("role") != "implementer" {
			t.Errorf("query role = %q, want implementer", r.URL.Query().Get("role"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": page})
	})
	c := newRegistryClient(t, h)
	got, err := c.Registry().Search(context.Background(), registry.KindAgent, registry.Filter{
		Role: "implementer",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestRegistryClient_Search_EmptyResultIsNonNil(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": []registry.Profile{}})
	})
	c := newRegistryClient(t, h)
	got, err := c.Registry().Search(context.Background(), registry.KindAgent, registry.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got == nil {
		t.Error("empty result returned nil; want non-nil zero-length slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// ─── UpdateSelf ─────────────────────────────────────────────────────────

func TestRegistryClient_UpdateSelf_Happy(t *testing.T) {
	urn := "msg://agent/agent-mux/agt_patcher0001"
	out := registry.Profile{URN: urn, Kind: registry.KindAgent, DisplayName: "Patched"}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %q, want PATCH", r.Method)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	c := newRegistryClient(t, h)
	newRole := "reviewer"
	got, err := c.Registry().UpdateSelf(context.Background(), urn,
		registry.UpdatePatch{Role: &newRole, LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("UpdateSelf: %v", err)
	}
	if got.URN != urn {
		t.Errorf("URN = %q, want %q", got.URN, urn)
	}
}

func TestRegistryClient_UpdateSelf_InvalidRequest(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"last_updated_by required"}}`))
	})
	c := newRegistryClient(t, h)
	_, err := c.Registry().UpdateSelf(context.Background(),
		"msg://agent/agent-mux/agt_x", registry.UpdatePatch{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Errorf("not wrapping ErrInvalidRequest: %v", err)
	}
}

// ─── Deregister ────────────────────────────────────────────────────────

func TestRegistryClient_Deregister_Happy(t *testing.T) {
	urn := "msg://agent/agent-mux/agt_dereg00001"
	out := registry.Profile{URN: urn, Kind: registry.KindAgent, Status: registry.StatusDeprecated}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	c := newRegistryClient(t, h)
	got, err := c.Registry().Deregister(context.Background(), urn)
	if err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if got.Status != registry.StatusDeprecated {
		t.Errorf("Status = %q, want deprecated", got.Status)
	}
}

func TestRegistryClient_Deregister_NotFound(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"missing"}}`))
	})
	c := newRegistryClient(t, h)
	_, err := c.Registry().Deregister(context.Background(), "msg://agent/agent-mux/agt_x")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("not ErrNotFound: %v", err)
	}
}

// ─── Sync ───────────────────────────────────────────────────────────────

func TestRegistryClient_Sync_200(t *testing.T) {
	urn := "msg://agent/agent-mux/agt_sync00001"
	out := registry.Profile{URN: urn, Kind: registry.KindAgent, DisplayName: "Synced"}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.EscapedPath(), "/sync") {
			t.Errorf("path missing /sync suffix: %s", r.URL.EscapedPath())
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	c := newRegistryClient(t, h)
	got, synced, err := c.Registry().Sync(context.Background(), urn)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !synced {
		t.Error("synced=false, want true on 200")
	}
	if got.URN != urn {
		t.Errorf("URN = %q, want %q", got.URN, urn)
	}
}

func TestRegistryClient_Sync_204(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := newRegistryClient(t, h)
	got, synced, err := c.Registry().Sync(context.Background(), "msg://agent/agent-mux/agt_x")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if synced {
		t.Error("synced=true on 204; want false")
	}
	if got.URN != "" {
		t.Errorf("got Profile populated on 204: %v", got)
	}
}

func TestRegistryClient_Sync_500(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"resolver blew up"}}`))
	})
	c := newRegistryClient(t, h)
	_, _, err := c.Registry().Sync(context.Background(), "msg://agent/agent-mux/agt_x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "resolver blew up") {
		t.Errorf("error missing body: %v", err)
	}
}

// ─── Unreachable ─────────────────────────────────────────────────────────

func TestRegistryClient_Unreachable(t *testing.T) {
	// Bind a listener, close it immediately, point the client at the
	// freshly-vacated port. The first call should surface
	// ErrDaemonUnreachable via the existing wrapIfUnreachable path.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	c := &Client{
		baseURL: "http://" + addr,
		http:    http.DefaultClient,
	}
	_, err = c.Registry().Lookup(context.Background(), "msg://agent/agent-mux/agt_x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("not ErrDaemonUnreachable: %v", err)
	}
}
