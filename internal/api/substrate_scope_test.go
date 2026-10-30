package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/testutil/migrateddb"
)

type substrateHarness struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []string
}

func (h *substrateHarness) seen(event string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.events, event)
}

var substratePrincipals = map[string]identity.Principal{
	"operator": {ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}, Addresses: []string{identity.OperatorID}},
	"nanite": {ID: "msg://service/nanite-registry/dev", Kind: "service", Verified: true, TokenID: "j1",
		Scopes: []string{"tether.registry:nanite"}, Addresses: []string{"msg://service/nanite-registry/dev"}},
	"plain": {ID: "msg://service/other/dev", Kind: "service", Verified: true, TokenID: "j2",
		Scopes: []string{"tether.registry"}, Addresses: []string{"msg://service/other/dev"}},
	"naniteSender": {ID: "msg://service/nanite/dev", Kind: "service", Verified: true, TokenID: "j3",
		Scopes: []string{"tether.messages", "tether.send_as:nanite"}, Addresses: []string{"msg://service/nanite/dev"}},
	"plainSender": {ID: "msg://service/plain/dev", Kind: "service", Verified: true, TokenID: "j4",
		Scopes: []string{"tether.messages"}, Addresses: []string{"msg://service/plain/dev"}},
}

func newSubstrateHarness(t *testing.T, mode identity.Mode) (*substrateHarness, *registry.Service) {
	t.Helper()
	reg := registry.NewService(registry.NewStorage(migrateddb.Open(t)))
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := &fakeLaunchService{
		getRes:          map[string]*store.SessionRow{"w": {ID: "w", State: string(session.StateRunning), LogicalAgentID: "worker"}},
		runtimeHealthOK: map[string]bool{"w": true},
	}
	h := &substrateHarness{}
	api := NewHandler(Deps{Service: svc, Registry: reg, MessageStore: db.MessagingStore(), IdentityMode: mode,
		IdentityAudit: func(_ context.Context, o identity.Observation) error {
			h.mu.Lock()
			h.events = append(h.events, o.Event)
			h.mu.Unlock()
			return nil
		}})
	gated := AgentScopeMiddleware(mode, api)
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := substratePrincipals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		gated.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h, reg
}

func (h *substrateHarness) do(t *testing.T, method, path, principal string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("X-Test-Principal", principal)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func itemPath(urn string) string { return "/registry/agents/" + url.PathEscape(urn) }

// CW-20261025-0001: tether.registry:nanite reaches only nanite actors.
func TestSubstrateLimitedRegistryWrites(t *testing.T) {
	h, reg := newSubstrateHarness(t, identity.Enforce)
	ctx := context.Background()
	foreign, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Not Nanite"})
	if err != nil {
		t.Fatal(err)
	}

	code, mine := h.do(t, "POST", "/registry/agents", "nanite", map[string]any{"display_name": "N1", "external_key": "nanite/i1/h1"})
	if code != http.StatusCreated {
		t.Fatalf("keyed nanite register = %d %v", code, mine)
	}
	naniteURN, _ := mine["urn"].(string)
	if code, _ := h.do(t, "POST", "/registry/agents", "nanite", map[string]any{"display_name": "N1", "external_key": "nanite/i1/h1"}); code != http.StatusOK {
		t.Fatalf("idempotent repeat = %d", code)
	}
	for name, body := range map[string]map[string]any{
		"other substrate": {"display_name": "X", "external_key": "tether/x"},
		"unkeyed":         {"display_name": "X"},
	} {
		if code, _ := h.do(t, "POST", "/registry/agents", "nanite", body); code != http.StatusForbidden {
			t.Errorf("%s register = %d, want 403", name, code)
		}
	}

	patch := map[string]any{"last_updated_by": "x", "scalars": map[string]any{"title": "t"}}
	if code, body := h.do(t, "PATCH", itemPath(naniteURN), "nanite", patch); code != http.StatusOK {
		t.Fatalf("patch own = %d %v", code, body)
	}
	if code, _ := h.do(t, "PATCH", itemPath(foreign.URN), "nanite", patch); code != http.StatusForbidden {
		t.Errorf("patch foreign = %d, want 403", code)
	}
	if code, _ := h.do(t, "DELETE", itemPath(foreign.URN), "nanite", nil); code != http.StatusForbidden {
		t.Errorf("delete foreign = %d, want 403", code)
	}
	if got, _ := reg.Lookup(ctx, foreign.URN); got.Status == registry.StatusDeprecated {
		t.Fatal("a foreign actor was deprecated by a nanite-limited credential")
	}
	if code, _ := h.do(t, "GET", "/registry/agents", "nanite", nil); code != http.StatusOK {
		t.Errorf("read = %d, want 200", code)
	}
	// Routes the qualifier does not cover stay with the unrestricted scope.
	if code, _ := h.do(t, "POST", itemPath(naniteURN)+"/sync", "nanite", nil); code != http.StatusForbidden {
		t.Errorf("sync = %d, want 403", code)
	}
	if code, _ := h.do(t, "POST", "/registry/reonboard", "nanite", nil); code != http.StatusForbidden {
		t.Errorf("reonboard = %d, want 403", code)
	}
	if code, _ := h.do(t, "DELETE", itemPath(naniteURN), "nanite", nil); code != http.StatusOK {
		t.Errorf("delete own = %d, want 200", code)
	}
	// An unrestricted holder and the operator are unchanged.
	if code, _ := h.do(t, "DELETE", itemPath(foreign.URN), "plain", nil); code != http.StatusOK {
		t.Errorf("unrestricted delete = %d, want 200", code)
	}
}

func TestSubstrateLimitIsRecordedNotRefusedInObserveMode(t *testing.T) {
	h, reg := newSubstrateHarness(t, identity.Observe)
	foreign, _ := reg.Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: "Not Nanite"})
	if code, _ := h.do(t, "PATCH", itemPath(foreign.URN), "nanite", map[string]any{"last_updated_by": "x", "scalars": map[string]any{"title": "t"}}); code != http.StatusOK {
		t.Fatalf("observe-mode patch = %d, want 200 (recorded, not refused)", code)
	}
	if !h.seen(eventSubstrateUnverified) {
		t.Fatalf("no %s event: %v", eventSubstrateUnverified, h.events)
	}
}

// CW-20261024-0023: tether.send_as:nanite authors as nanite actors only.
func TestSubstrateLimitedSendAs(t *testing.T) {
	h, reg := newSubstrateHarness(t, identity.Enforce)
	ctx := context.Background()
	nanite, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "N"}, "nanite", "i/h")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := reg.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "O"}, "tether", "o1")
	if err != nil {
		t.Fatal(err)
	}
	send := func(principal, from string) int {
		code, _ := h.do(t, "POST", "/messages", principal, map[string]any{"from": from, "to": "msg://agent/local/worker", "kind": "notice"})
		return code
	}
	if code := send("naniteSender", nanite.URN); code != http.StatusCreated {
		t.Errorf("send as nanite actor = %d, want 201", code)
	}
	if code := send("naniteSender", other.URN); code != http.StatusForbidden {
		t.Errorf("send as non-nanite actor = %d, want 403", code)
	}
	if code := send("plainSender", nanite.URN); code != http.StatusForbidden {
		t.Errorf("send as nanite actor without the grant = %d, want 403", code)
	}
	if code := send("naniteSender", "msg://agent/agent-mux/agt_not_registered"); code != http.StatusForbidden {
		t.Errorf("send as unregistered actor = %d, want 403", code)
	}
}
