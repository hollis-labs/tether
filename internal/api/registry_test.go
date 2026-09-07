package api

// registry_test.go — end-to-end HTTP coverage for the /registry/ tree
// (T-v060-01-05). The tests wire a real *registry.Service over a real
// *registry.Storage on an in-memory SQLite DB and exercise the HTTP
// surface via httptest.NewServer. This is the same shape as
// internal/registry/service_test.go's integration tests, lifted up one
// layer.
//
// Coverage matrix (mirrors the sprint's acceptance criteria):
//
//   - POST /registry/{kind} happy path (agent + project) + caller-URN
//     rejection + malformed JSON + empty display_name + unsupported kind.
//   - GET single: 200 + Profile; 404 on missing.
//   - GET list: empty result is `{"agents":[]}` not null; each filter
//     alone + combined; status=deprecated; default excludes deprecated;
//     status=* returns both.
//   - PATCH happy (scalar + array replace/append/remove) + missing
//     last_updated_by (400) + unknown URN (404).
//   - DELETE happy (returns deprecated Profile) + unknown URN (404) +
//     child rows untouched.
//   - POST sync: 204 if no callback; 200 + refreshed Profile when a
//     file:// callback is present.
//   - Method-not-allowed coverage for PUT.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// newRegistryHandler stands up a real Service+Storage on an in-memory
// SQLite and wires it into the api Handler. opts let individual tests
// register Sync resolvers (file://) when needed.
func newRegistryHandler(t *testing.T, opts ...registry.ServiceOption) (http.Handler, *registry.Service) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage, opts...)
	return NewHandler(Deps{Registry: svc}), svc
}

// regServer wraps a httptest.Server + a typed-helper "do" method so
// individual test bodies stay short.
type regServer struct {
	t   *testing.T
	srv *httptest.Server
	svc *registry.Service
}

func newRegServer(t *testing.T, opts ...registry.ServiceOption) *regServer {
	t.Helper()
	h, svc := newRegistryHandler(t, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &regServer{t: t, srv: srv, svc: svc}
}

// do issues an HTTP request and returns the *http.Response + raw body.
// Body may be nil; if non-nil it's marshaled as JSON.
func (r *regServer) do(method, path string, body any) (*http.Response, []byte) {
	r.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, r.srv.URL+path, reader)
	if err != nil {
		r.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("read body: %v", err)
	}
	return resp, buf
}

// itemURL builds the URL-escaped item path for the given urn.
func itemURL(kind, urn string) string {
	return "/registry/" + kind + "/" + url.PathEscape(urn)
}

// ─── POST /registry/{kind} ────────────────────────────────────────────────────

func TestRegistry_Register_AgentHappy(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   "Alpha Agent",
		Role:          "implementer",
		LastUpdatedBy: "tester",
		Capabilities:  []string{"go", "sqlite"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	var p registry.Profile
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode body: %v: %s", err, body)
	}
	if !strings.HasPrefix(p.URN, "msg://agent/agent-mux/agt_") {
		t.Errorf("URN = %q, want msg://agent/agent-mux/agt_ prefix", p.URN)
	}
	if p.Kind != registry.KindAgent {
		t.Errorf("Kind = %q, want agent", p.Kind)
	}
	if p.Status != registry.StatusActive {
		t.Errorf("Status = %q, want active", p.Status)
	}
	if len(p.Capabilities) != 2 {
		t.Errorf("Capabilities = %v, want 2 items", p.Capabilities)
	}
}

func TestRegistry_Register_ProjectHappy(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/projects", registry.Profile{
		DisplayName:   "Tether",
		LastUpdatedBy: "tester",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var p registry.Profile
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(p.URN, "msg://agent/agent-mux/prj_") {
		t.Errorf("URN = %q, want prj_ prefix", p.URN)
	}
	if p.Kind != registry.KindProject {
		t.Errorf("Kind = %q", p.Kind)
	}
}

func TestRegistry_Register_RejectsCallerURN(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		URN:         "msg://agent/agent-mux/agt_pleasekeep",
		DisplayName: "Caller-Provided",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"code":"invalid_request"`) {
		t.Errorf("body missing invalid_request code: %s", body)
	}
}

func TestRegistry_Register_MalformedJSON(t *testing.T) {
	r := newRegServer(t)
	// Send raw garbage so the json decoder fails.
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/registry/agents",
		bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRegistry_Register_EmptyDisplayName(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
}

func TestRegistry_Register_UnsupportedKind(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodPost, "/registry/services", registry.Profile{
		DisplayName: "Nope",
	})
	// Plural-kind segment is the router's responsibility; unknown
	// segments return 404 (the route's "unknown registry kind" branch).
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ─── GET single / GET list ────────────────────────────────────────────────────

func TestRegistry_Lookup_Found(t *testing.T) {
	r := newRegServer(t)
	created := registerOne(t, r, "agents", "Alpha", "implementer")

	resp, body := r.do(http.MethodGet, itemURL("agents", created.URN), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var got registry.Profile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.URN != created.URN {
		t.Errorf("URN = %q, want %q", got.URN, created.URN)
	}
}

func TestRegistry_Lookup_NotFound(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodGet,
		itemURL("agents", "msg://agent/agent-mux/agt_missing000"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"code":"not_found"`) {
		t.Errorf("body missing not_found code: %s", body)
	}
}

func TestRegistry_LookupBy_ExternalID(t *testing.T) {
	r := newRegServer(t)
	created := registerOne(t, r, "projects", "Clockwork", "")
	if err := r.svc.AttachExternalID(context.Background(), created.URN, "cerberus", "clockwork"); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}

	resp, body := r.do(http.MethodGet, "/registry/projects?external_id=clockwork&substrate=cerberus", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var env struct {
		Project registry.Profile `json:"project"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Project.URN != created.URN {
		t.Fatalf("lookup-by urn = %q, want %q", env.Project.URN, created.URN)
	}
}

func TestRegistry_Search_EmptyReturnsEmptyArray(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodGet, "/registry/agents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	// Verify the JSON shape: agents key carries a non-nil, zero-length array.
	var env struct {
		Agents []registry.Profile `json:"agents"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if env.Agents == nil {
		t.Errorf("empty result decoded as nil; want non-nil zero-length slice (body=%s)", body)
	}
	if len(env.Agents) != 0 {
		t.Errorf("len = %d, want 0", len(env.Agents))
	}
}

func TestRegistry_Search_Filters(t *testing.T) {
	r := newRegServer(t)
	alpha := registerWithCapabilities(t, r, "agents", "Alpha", "implementer", "tether", []string{"go"})
	beta := registerWithCapabilities(t, r, "agents", "Beta", "reviewer", "tether", []string{"go", "rust"})
	gamma := registerWithCapabilities(t, r, "agents", "Gamma", "implementer", "other", []string{"rust"})

	// role=implementer narrows to alpha+gamma.
	resp, body := r.do(http.MethodGet, "/registry/agents?role=implementer", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	got := decodeSearchAgents(t, body)
	if len(got) != 2 {
		t.Fatalf("role=implementer: got %d, want 2 (body=%s)", len(got), body)
	}
	if got[0].DisplayName != "Alpha" || got[1].DisplayName != "Gamma" {
		t.Errorf("alphabetical order broken: %q, %q", got[0].DisplayName, got[1].DisplayName)
	}

	// project=tether narrows to alpha+beta.
	_, body = r.do(http.MethodGet, "/registry/agents?project=tether", nil)
	got = decodeSearchAgents(t, body)
	if len(got) != 2 {
		t.Fatalf("project=tether: got %d, want 2", len(got))
	}

	// capability=rust narrows to beta+gamma.
	_, body = r.do(http.MethodGet, "/registry/agents?capability=rust", nil)
	got = decodeSearchAgents(t, body)
	if len(got) != 2 {
		t.Fatalf("capability=rust: got %d, want 2", len(got))
	}

	// Combined role=implementer&capability=go narrows to alpha only.
	_, body = r.do(http.MethodGet, "/registry/agents?role=implementer&capability=go", nil)
	got = decodeSearchAgents(t, body)
	if len(got) != 1 || got[0].DisplayName != "Alpha" {
		t.Fatalf("combined filter: got %v", got)
	}

	_ = beta
	_ = alpha
	_ = gamma
}

func TestRegistry_Search_StatusDeprecated(t *testing.T) {
	r := newRegServer(t)
	alive := registerOne(t, r, "agents", "Live", "x")
	soft := registerOne(t, r, "agents", "Soft", "x")
	if _, err := r.svc.Deregister(context.Background(), soft.URN); err != nil {
		t.Fatalf("deregister: %v", err)
	}

	// Default search excludes deprecated.
	_, body := r.do(http.MethodGet, "/registry/agents", nil)
	got := decodeSearchAgents(t, body)
	if len(got) != 1 || got[0].URN != alive.URN {
		t.Errorf("default search returned %d rows, want only Live: %v", len(got), got)
	}

	// status=deprecated returns only the soft-deleted row.
	_, body = r.do(http.MethodGet, "/registry/agents?status=deprecated", nil)
	got = decodeSearchAgents(t, body)
	if len(got) != 1 || got[0].URN != soft.URN {
		t.Errorf("status=deprecated returned %v", got)
	}

	// status=* returns both.
	_, body = r.do(http.MethodGet, "/registry/agents?status=*", nil)
	got = decodeSearchAgents(t, body)
	if len(got) != 2 {
		t.Errorf("status=* returned %d, want 2", len(got))
	}
}

// ─── PATCH ──────────────────────────────────────────────────────────────────

func TestRegistry_UpdateSelf_ScalarAndArrayMerges(t *testing.T) {
	r := newRegServer(t)
	created := registerWithCapabilities(t, r, "agents", "Patcher", "implementer", "tether", []string{"go"})

	newRole := "reviewer"
	patch := registry.UpdatePatch{
		Role:          &newRole,
		LastUpdatedBy: "tester",
		// REPLACE capabilities entirely.
		Capabilities: &registry.ArrayPatch[string]{
			Mode:  registry.ArrayModeReplace,
			Value: []string{"rust"},
		},
	}
	resp, body := r.do(http.MethodPatch, itemURL("agents", created.URN), patch)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var got registry.Profile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Role != "reviewer" {
		t.Errorf("Role = %q, want reviewer", got.Role)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "rust" {
		t.Errorf("Capabilities after replace = %v, want [rust]", got.Capabilities)
	}

	// APPEND a capability.
	appendPatch := registry.UpdatePatch{
		LastUpdatedBy: "tester",
		Capabilities: &registry.ArrayPatch[string]{
			Mode:  registry.ArrayModeAppend,
			Value: []string{"sqlite"},
		},
	}
	_, body = r.do(http.MethodPatch, itemURL("agents", created.URN), appendPatch)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Capabilities) != 2 {
		t.Errorf("Capabilities after append = %v, want 2", got.Capabilities)
	}

	// REMOVE a capability.
	removePatch := registry.UpdatePatch{
		LastUpdatedBy: "tester",
		Capabilities: &registry.ArrayPatch[string]{
			Mode:  registry.ArrayModeRemove,
			Value: []string{"rust"},
		},
	}
	_, body = r.do(http.MethodPatch, itemURL("agents", created.URN), removePatch)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "sqlite" {
		t.Errorf("Capabilities after remove = %v, want [sqlite]", got.Capabilities)
	}
}

func TestRegistry_UpdateSelf_MissingLastUpdatedBy(t *testing.T) {
	r := newRegServer(t)
	created := registerOne(t, r, "agents", "NoUpdater", "x")
	newRole := "reviewer"
	// LastUpdatedBy omitted on purpose.
	resp, body := r.do(http.MethodPatch, itemURL("agents", created.URN),
		registry.UpdatePatch{Role: &newRole})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
}

func TestRegistry_UpdateSelf_UnknownURN(t *testing.T) {
	r := newRegServer(t)
	newRole := "reviewer"
	resp, body := r.do(http.MethodPatch,
		itemURL("agents", "msg://agent/agent-mux/agt_nothere0001"),
		registry.UpdatePatch{Role: &newRole, LastUpdatedBy: "tester"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
	}
}

// ─── DELETE ────────────────────────────────────────────────────────────────

func TestRegistry_Deregister_Happy(t *testing.T) {
	r := newRegServer(t)
	created := registerWithCapabilities(t, r, "agents", "ToDelete", "x", "p", []string{"go"})

	resp, body := r.do(http.MethodDelete, itemURL("agents", created.URN), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var got registry.Profile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != registry.StatusDeprecated {
		t.Errorf("Status = %q, want deprecated", got.Status)
	}
	// Child rows are untouched per D11 — confirm a GET still returns
	// capabilities even though the row is soft-deleted.
	_, body = r.do(http.MethodGet, itemURL("agents", created.URN), nil)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "go" {
		t.Errorf("post-delete capabilities = %v, want [go]", got.Capabilities)
	}
}

func TestRegistry_Deregister_NotFound(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodDelete,
		itemURL("agents", "msg://agent/agent-mux/agt_ghost00000"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRegistry_Merge_Happy(t *testing.T) {
	r := newRegServer(t)
	src := registerWithCapabilities(t, r, "projects", "Clockwork Draft", "", "tether", []string{"go"})
	dst := registerWithCapabilities(t, r, "projects", "Clockwork", "", "tether", []string{"sqlite"})
	if err := r.svc.AttachExternalID(context.Background(), src.URN, "cerberus", "clockwork"); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}

	resp, body := r.do(http.MethodPost, itemURL("projects", src.URN)+"/merge", map[string]string{"into": dst.URN})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var merged registry.Profile
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatalf("decode merged: %v", err)
	}
	if merged.URN != dst.URN {
		t.Fatalf("merged urn = %q, want %q", merged.URN, dst.URN)
	}
	if len(merged.ExternalIDs) == 0 {
		t.Fatalf("merged external_ids empty")
	}

	srcAfter, err := r.svc.Lookup(context.Background(), src.URN)
	if err != nil {
		t.Fatalf("lookup src after merge: %v", err)
	}
	if srcAfter.Status != registry.StatusMerged || srcAfter.MergedInto != dst.URN {
		t.Fatalf("src after merge = %+v", srcAfter)
	}
}

// ─── POST /sync ────────────────────────────────────────────────────────────

func TestRegistry_Sync_NoCallbackReturns204(t *testing.T) {
	r := newRegServer(t)
	created := registerOne(t, r, "agents", "NoCallback", "x")
	resp, body := r.do(http.MethodPost, itemURL("agents", created.URN)+"/sync", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.StatusCode, body)
	}
	if len(body) != 0 {
		t.Errorf("204 body should be empty, got %q", body)
	}
}

func TestRegistry_Sync_FileCallbackRefreshes(t *testing.T) {
	// Build a file:// fixture under a temp catalog root, then wire a
	// FileResolver pointing at that root.
	tmp := t.TempDir()
	fxPath := filepath.Join(tmp, "agent.yaml")
	if err := os.WriteFile(fxPath, []byte(`{
  "display_name": "Refreshed Name",
  "role": "synced-role",
  "capabilities": ["after-sync"]
}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resolver, err := registry.NewFileResolver(tmp)
	if err != nil {
		t.Fatalf("file resolver: %v", err)
	}

	r := newRegServer(t, registry.WithResolver(resolver))

	// Seed an agent with a file:// callback pointing at the fixture.
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   "Pre-Sync Name",
		Role:          "pre",
		LastUpdatedBy: "tester",
		Callback: &registry.Callback{
			Scheme: "file",
			Target: "file://" + fxPath,
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d: %s", resp.StatusCode, body)
	}
	var seeded registry.Profile
	if err := json.Unmarshal(body, &seeded); err != nil {
		t.Fatalf("decode register: %v", err)
	}

	// Sync should overwrite the thin profile from the fixture.
	resp, body = r.do(http.MethodPost, itemURL("agents", seeded.URN)+"/sync", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync status = %d: %s", resp.StatusCode, body)
	}
	var refreshed registry.Profile
	if err := json.Unmarshal(body, &refreshed); err != nil {
		t.Fatalf("decode sync: %v", err)
	}
	if refreshed.DisplayName != "Refreshed Name" {
		t.Errorf("DisplayName = %q, want Refreshed Name", refreshed.DisplayName)
	}
	if refreshed.Role != "synced-role" {
		t.Errorf("Role = %q, want synced-role", refreshed.Role)
	}
	if len(refreshed.Capabilities) != 1 || refreshed.Capabilities[0] != "after-sync" {
		t.Errorf("Capabilities = %v, want [after-sync]", refreshed.Capabilities)
	}
	if refreshed.CachedAt == nil {
		t.Errorf("CachedAt nil after sync; bump expected")
	}
}

// ─── Method-not-allowed ────────────────────────────────────────────────────

func TestRegistry_MethodNotAllowed(t *testing.T) {
	r := newRegServer(t)
	created := registerOne(t, r, "agents", "M", "x")
	resp, body := r.do(http.MethodPut, itemURL("agents", created.URN), nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"code":"method_not_allowed"`) {
		t.Errorf("body missing method_not_allowed code: %s", body)
	}
}

func TestRegistry_MethodNotAllowed_OnCollection(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodPut, "/registry/agents", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// ─── T09 acceptance #3: privacy redaction on public discovery ─────────────

func TestRegistry_Lookup_RedactsCallbackHostAddressKindMetaAndExternalIDs(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   "Secretive",
		LastUpdatedBy: "tester",
		Callback:      &registry.Callback{Scheme: "file", Target: "file:///Users/tester/.tether/catalog/agents/secretive.yaml"},
		KindMeta:      json.RawMessage(`{"internal_note":"do not leak"}`),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status = %d: %s", resp.StatusCode, body)
	}
	var created registry.Profile
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	// Register's own response is the caller acting on their own
	// just-submitted data, not browsing someone else's -- full fidelity
	// is correct there and is NOT what this test is about.
	if created.Callback == nil || len(created.KindMeta) == 0 {
		t.Fatalf("register response unexpectedly redacted: %+v", created)
	}

	hostAddr := "10.0.0.7:9999"
	if _, err := r.svc.UpdateSelf(context.Background(), created.URN, registry.UpdatePatch{
		HostAddress: &hostAddr, LastUpdatedBy: "tester",
	}); err != nil {
		t.Fatalf("UpdateSelf host_address: %v", err)
	}
	if err := r.svc.AttachExternalID(context.Background(), created.URN, "cerberus", "secret-provider-id"); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}

	resp, body = r.do(http.MethodGet, itemURL("agents", created.URN), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup: status = %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)

	var got registry.Profile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if got.DisplayName != "Secretive" {
		t.Errorf("display_name = %q, want the public field preserved", got.DisplayName)
	}
}

func TestRegistry_Search_RedactsCallbackHostAddressKindMetaAndExternalIDs(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   "Searchable Secretive",
		LastUpdatedBy: "tester",
		Callback:      &registry.Callback{Scheme: "cli", Target: "/usr/local/bin/launch-with-secrets --token=abc123"},
		KindMeta:      json.RawMessage(`{"api_key":"shh"}`),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status = %d: %s", resp.StatusCode, body)
	}
	var created registry.Profile
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if err := r.svc.AttachExternalID(context.Background(), created.URN, "cerberus", "secret-provider-id"); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}

	resp, body = r.do(http.MethodGet, "/registry/agents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: status = %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)

	var found bool
	for _, a := range decodeSearchAgents(t, body) {
		if a.URN != created.URN {
			continue
		}
		found = true
		if a.DisplayName != "Searchable Secretive" {
			t.Errorf("display_name = %q, want the public field preserved", a.DisplayName)
		}
	}
	if !found {
		t.Fatalf("created agent missing from search results")
	}

	// LookupBy — the external_id query-param branch of the same handler
	// — is redacted the same way.
	resp, body = r.do(http.MethodGet, "/registry/agents?external_id=secret-provider-id&substrate=cerberus", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup_by: status = %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)
}

// assertNoLeakedRegistryFields fails the test if any of the four fields
// T09 acceptance #3 targets for redaction (callback, host_address,
// kind_meta, external_ids) appear anywhere in a "public discovery"
// response body. A raw substring check against the wire JSON keys and
// secret values, rather than unmarshaling into registry.Profile and
// checking for zero fields — a struct-level check can't tell "redacted"
// apart from "never set," and would keep passing even if a future change
// silently reintroduced the field under a different Go field ordering.
func assertNoLeakedRegistryFields(t *testing.T, body []byte) {
	t.Helper()
	for _, key := range []string{`"callback"`, `"host_address"`, `"kind_meta"`, `"external_ids"`} {
		if bytes.Contains(body, []byte(key)) {
			t.Errorf("response leaks redacted field %s: %s", key, body)
		}
	}
	for _, secret := range []string{"do not leak", "shh", "secret-provider-id", "10.0.0.7", "launch-with-secrets", "abc123"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("response leaks secret value %q: %s", secret, body)
		}
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

// registerOne posts a minimal Profile and returns the canonical body. Test
// helper to keep callsites short.
func registerOne(t *testing.T, r *regServer, kindSeg, name, role string) registry.Profile {
	t.Helper()
	return registerWithCapabilities(t, r, kindSeg, name, role, "tether", nil)
}

func registerWithCapabilities(t *testing.T, r *regServer, kindSeg, name, role, project string, caps []string) registry.Profile {
	t.Helper()
	resp, body := r.do(http.MethodPost, "/registry/"+kindSeg, registry.Profile{
		DisplayName:   name,
		Role:          role,
		Project:       project,
		LastUpdatedBy: "tester",
		Capabilities:  caps,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %q: status = %d, body=%s", name, resp.StatusCode, body)
	}
	var p registry.Profile
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode %q: %v", name, err)
	}
	return p
}

func decodeSearchAgents(t *testing.T, body []byte) []registry.Profile {
	t.Helper()
	var env struct {
		Agents []registry.Profile `json:"agents"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode search: %v: %s", err, body)
	}
	return env.Agents
}

// Sanity check that the route uses the older not-found behavior for an
// empty trailing segment. Mostly here to lock the invariant — if someone
// later swaps to Go 1.22 patterns, this test surfaces the regression.
func TestRegistry_EmptyKindSegment(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodGet, "/registry/", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}
