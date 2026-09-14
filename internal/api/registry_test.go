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
	"time"

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
	assertNoLeakedRegistryFields(t, body)
	var merged registry.Profile
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatalf("decode merged: %v", err)
	}
	if merged.URN != dst.URN {
		t.Fatalf("merged urn = %q, want %q", merged.URN, dst.URN)
	}

	exts, err := r.svc.LookupExternalIDsForURN(context.Background(), dst.URN)
	if err != nil {
		t.Fatalf("LookupExternalIDsForURN: %v", err)
	}
	if len(exts) == 0 {
		t.Fatalf("merged external_ids empty in storage")
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
  "project": "synced-project",
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

	// Sync should update derived fields but NEVER touch authored fields (CW-20260912-0095).
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
	if refreshed.Project != "synced-project" {
		t.Errorf("Project = %q, want synced-project", refreshed.Project)
	}
	if refreshed.Role != "pre" {
		t.Errorf("Role = %q, want pre (authored field must not be overwritten)", refreshed.Role)
	}
	if len(refreshed.Capabilities) != 0 {
		t.Errorf("Capabilities = %v, want empty (authored field must not be overwritten)", refreshed.Capabilities)
	}
	if refreshed.CachedAt == nil {
		t.Errorf("CachedAt nil after sync; bump expected")
	}
}

func TestRegistry_Sync_FailureReportsBadGatewayAndLeavesLastGood(t *testing.T) {
	tmp := t.TempDir()
	fxPath := filepath.Join(tmp, "corrupt_agent.json")
	if err := os.WriteFile(fxPath, []byte(`{INVALID JSON`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resolver, err := registry.NewFileResolver(tmp)
	if err != nil {
		t.Fatalf("file resolver: %v", err)
	}

	r := newRegServer(t, registry.WithResolver(resolver))

	// Seed agent with initial values
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   "Last Good Name",
		Description:   "Preserved description",
		Role:          "specialist",
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

	// Sync fails due to invalid JSON payload -> 502 Bad Gateway
	resp, body = r.do(http.MethodPost, itemURL("agents", seeded.URN)+"/sync", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("sync status = %d, want 502: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"code":"internal_error"`) {
		t.Errorf("expected internal_error in error body: %s", body)
	}

	// Reading the profile shows last-good values are completely intact
	resp, body = r.do(http.MethodGet, itemURL("agents", seeded.URN), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", resp.StatusCode, body)
	}
	var current registry.Profile
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if current.DisplayName != "Last Good Name" {
		t.Errorf("DisplayName = %q, want Last Good Name", current.DisplayName)
	}
	if current.Description != "Preserved description" {
		t.Errorf("Description = %q, want Preserved description", current.Description)
	}
	if current.Role != "specialist" {
		t.Errorf("Role = %q, want specialist", current.Role)
	}
	if current.CachedAt != nil {
		t.Errorf("CachedAt = %v, want nil (sync never succeeded)", current.CachedAt)
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
	// Under bidirectional redaction (CW-20260912-0053), Register's default response
	// is redacted to prevent using writes as an un-audited leak vector.
	assertNoLeakedRegistryFields(t, body)

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

	// Legitimate read path: ?full=true returns the unredacted profile.
	respFull, bodyFull := r.do(http.MethodGet, itemURL("agents", created.URN)+"?full=true", nil)
	if respFull.StatusCode != http.StatusOK {
		t.Fatalf("lookup with full=true: status = %d: %s", respFull.StatusCode, bodyFull)
	}
	var gotFull registry.Profile
	if err := json.Unmarshal(bodyFull, &gotFull); err != nil {
		t.Fatalf("decode lookup full: %v", err)
	}
	if gotFull.Callback == nil || gotFull.HostAddress != hostAddr || len(gotFull.ExternalIDs) == 0 {
		t.Errorf("lookup with full=true did not expose sensitive fields: %+v", gotFull)
	}

	// Legitimate read path: ?include=callback selectively exposes callback only.
	respInc, bodyInc := r.do(http.MethodGet, itemURL("agents", created.URN)+"?include=callback", nil)
	if respInc.StatusCode != http.StatusOK {
		t.Fatalf("lookup with include=callback: status = %d: %s", respInc.StatusCode, bodyInc)
	}
	if !bytes.Contains(bodyInc, []byte(`"callback"`)) {
		t.Errorf("expected callback in response: %s", bodyInc)
	}
	if bytes.Contains(bodyInc, []byte(`"host_address"`)) || bytes.Contains(bodyInc, []byte(`"kind_meta"`)) || bytes.Contains(bodyInc, []byte(`"external_ids"`)) {
		t.Errorf("expected other sensitive fields to stay omitted: %s", bodyInc)
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

func TestRegistry_WriteRoutes_RedactedByDefault(t *testing.T) {
	fxDir := t.TempDir()
	fxPath := filepath.Join(fxDir, "agent.json")
	if err := os.WriteFile(fxPath, []byte(`{"display_name":"Synced Agent","role":"synced"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resolver, err := registry.NewFileResolver(fxDir)
	if err != nil {
		t.Fatalf("file resolver: %v", err)
	}

	r := newRegServer(t, registry.WithResolver(resolver))
	ctx := context.Background()

	// 1. POST /registry/agents (Register)
	hostAddr := "192.168.1.50:8080"
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   "Writable Agent",
		LastUpdatedBy: "tester",
		HostAddress:   hostAddr,
		Callback:      &registry.Callback{Scheme: "file", Target: "file://" + fxPath},
		KindMeta:      json.RawMessage(`{"secret_config":"dont-leak"}`),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)

	var p1 registry.Profile
	if err := json.Unmarshal(body, &p1); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if p1.URN == "" {
		t.Fatal("empty URN in register response")
	}

	if err := r.svc.AttachExternalID(ctx, p1.URN, "cerberus", "ext-secret-id"); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}

	// 2. PATCH /registry/agents/{urn} (Update)
	newTitle := "Updated Title"
	resp, body = r.do(http.MethodPatch, itemURL("agents", p1.URN), registry.UpdatePatch{
		Title:         &newTitle,
		LastUpdatedBy: "tester",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch: %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)

	// 3. POST /registry/agents/{urn}/sync (Sync)
	resp, body = r.do(http.MethodPost, itemURL("agents", p1.URN)+"/sync", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync: %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)

	// 4. POST /registry/projects/{urn}/merge (Merge)
	src := registerWithCapabilities(t, r, "projects", "Proj Src", "", "tether", nil)
	dst := registerWithCapabilities(t, r, "projects", "Proj Dst", "", "tether", nil)
	if err := r.svc.AttachExternalID(ctx, src.URN, "cerberus", "secret-merge-ext-id"); err != nil {
		t.Fatalf("AttachExternalID src: %v", err)
	}
	resp, body = r.do(http.MethodPost, itemURL("projects", src.URN)+"/merge", map[string]string{"into": dst.URN})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge: %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)

	// 5. DELETE /registry/agents/{urn} (Deregister)
	resp, body = r.do(http.MethodDelete, itemURL("agents", p1.URN), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d: %s", resp.StatusCode, body)
	}
	assertNoLeakedRegistryFields(t, body)

	// 6. Write route with ?full=true exposes all fields
	resp, body = r.do(http.MethodPost, "/registry/agents?full=true", registry.Profile{
		DisplayName:   "Full Writer",
		LastUpdatedBy: "tester",
		HostAddress:   hostAddr,
		Callback:      &registry.Callback{Scheme: "file", Target: "file://" + fxPath},
		KindMeta:      json.RawMessage(`{"secret_config":"dont-leak"}`),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register with full: %d: %s", resp.StatusCode, body)
	}
	var fullCreated registry.Profile
	if err := json.Unmarshal(body, &fullCreated); err != nil {
		t.Fatalf("decode full register: %v", err)
	}
	if fullCreated.Callback == nil || fullCreated.HostAddress != hostAddr || len(fullCreated.KindMeta) == 0 {
		t.Fatalf("register with ?full=true expected unredacted fields, got: %+v", fullCreated)
	}
}

func TestRegistry_OwnerProvenance(t *testing.T) {
	r := newRegServer(t)

	// 1. Register with Owner
	resp, body := r.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   "Cerberus Agent",
		Owner:         "cerberus",
		LastUpdatedBy: "tester",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d: %s", resp.StatusCode, body)
	}
	var created registry.Profile
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if created.Owner != "cerberus" {
		t.Errorf("Owner = %q, want cerberus", created.Owner)
	}

	// 2. Lookup preserves Owner in response
	resp, body = r.do(http.MethodGet, itemURL("agents", created.URN), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup status = %d: %s", resp.StatusCode, body)
	}
	var lookedUp registry.Profile
	if err := json.Unmarshal(body, &lookedUp); err != nil {
		t.Fatalf("decode lookup: %v", err)
	}
	if lookedUp.Owner != "cerberus" {
		t.Errorf("lookup Owner = %q, want cerberus", lookedUp.Owner)
	}

	// 3. Search includes Owner
	resp, body = r.do(http.MethodGet, "/registry/agents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d: %s", resp.StatusCode, body)
	}
	agents := decodeSearchAgents(t, body)
	found := false
	for _, a := range agents {
		if a.URN == created.URN {
			found = true
			if a.Owner != "cerberus" {
				t.Errorf("search Owner = %q, want cerberus", a.Owner)
			}
		}
	}
	if !found {
		t.Fatal("created agent not found in search")
	}

	// 4. Merge owner rules:
	// 4a. External callers win over Tether store default:
	//     src (cerberus) merged into dst (tether) -> dst becomes owned by cerberus
	srcCerb := registerWithCapabilities(t, r, "projects", "Cerberus Project", "", "tether", nil)
	dstTether := registerWithCapabilities(t, r, "projects", "Tether Project", "", "tether", nil)
	ownerCerb := "cerberus"
	ownerTether := "tether"
	if _, err := r.svc.UpdateSelf(context.Background(), srcCerb.URN, registry.UpdatePatch{Owner: &ownerCerb, LastUpdatedBy: "tester"}); err != nil {
		t.Fatalf("set srcCerb owner: %v", err)
	}
	if _, err := r.svc.UpdateSelf(context.Background(), dstTether.URN, registry.UpdatePatch{Owner: &ownerTether, LastUpdatedBy: "tester"}); err != nil {
		t.Fatalf("set dstTether owner: %v", err)
	}

	resp, body = r.do(http.MethodPost, itemURL("projects", srcCerb.URN)+"/merge", map[string]string{"into": dstTether.URN})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge cerberus into tether: %d: %s", resp.StatusCode, body)
	}
	dstAfter, err := r.svc.Lookup(context.Background(), dstTether.URN)
	if err != nil {
		t.Fatalf("lookup dstAfter: %v", err)
	}
	if dstAfter.Owner != "cerberus" {
		t.Errorf("merged dst owner = %q, want cerberus (external minter wins over tether store default)", dstAfter.Owner)
	}

	// 4b. Owner conflict: two distinct external minters (e.g. loom vs cerberus) -> conflict!
	srcLoom := registerWithCapabilities(t, r, "projects", "Loom Project", "", "tether", nil)
	ownerLoom := "loom"
	if _, err := r.svc.UpdateSelf(context.Background(), srcLoom.URN, registry.UpdatePatch{Owner: &ownerLoom, LastUpdatedBy: "tester"}); err != nil {
		t.Fatalf("set srcLoom owner: %v", err)
	}
	resp, body = r.do(http.MethodPost, itemURL("projects", srcLoom.URN)+"/merge", map[string]string{"into": dstTether.URN})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("merge loom into cerberus: expected 400 owner conflict, got %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("owner conflict")) {
		t.Errorf("expected owner conflict message in response: %s", body)
	}
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

func TestRegistry_CorrelationFields_HTTP(t *testing.T) {
	r := newRegServer(t)

	// 1. Create a project with tags, guidelines, entry_points
	createBody := map[string]any{
		"display_name": "API Correlation Project",
		"description":  "Demonstrating HTTP correlation fields",
		"tags":         []string{"api", "http", "tether"},
		"guidelines":   "Always check status code before reading body.",
		"entry_points": []string{"internal/api/server.go"},
	}
	resp, body := r.do(http.MethodPost, "/registry/projects", createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project: %d: %s", resp.StatusCode, body)
	}

	var created struct {
		URN           string                        `json:"urn"`
		Tags          []string                      `json:"tags"`
		Guidelines    string                        `json:"guidelines"`
		EntryPoints   []string                      `json:"entry_points"`
		FieldMetadata map[string]registry.FieldMeta `json:"field_metadata"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if len(created.Tags) != 3 || created.Tags[0] != "api" {
		t.Errorf("created.Tags = %v; want [api http tether]", created.Tags)
	}
	if created.Guidelines != "Always check status code before reading body." {
		t.Errorf("created.Guidelines = %q", created.Guidelines)
	}
	if len(created.EntryPoints) != 1 || created.EntryPoints[0] != "internal/api/server.go" {
		t.Errorf("created.EntryPoints = %v", created.EntryPoints)
	}
	if meta, ok := created.FieldMetadata["guidelines"]; !ok || meta.Class != registry.FieldClassAuthored {
		t.Errorf("guidelines FieldMetadata = %+v; want authored", meta)
	}

	// 2. Search with ?tag=tether
	resp, body = r.do(http.MethodGet, "/registry/projects?tag=tether", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search ?tag=tether: %d: %s", resp.StatusCode, body)
	}
	var searchRes struct {
		Projects []struct {
			URN string `json:"urn"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &searchRes); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(searchRes.Projects) != 1 || searchRes.Projects[0].URN != created.URN {
		t.Errorf("search ?tag=tether returned %v; want [%s]", searchRes.Projects, created.URN)
	}

	// 3. Search with ?tag=missing
	resp, body = r.do(http.MethodGet, "/registry/projects?tag=missing", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search ?tag=missing: %d: %s", resp.StatusCode, body)
	}
	searchRes.Projects = nil
	if err := json.Unmarshal(body, &searchRes); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(searchRes.Projects) != 0 {
		t.Errorf("search ?tag=missing returned %v; want empty", searchRes.Projects)
	}

	// 4. PATCH guidelines and tags
	patchBody := map[string]any{
		"guidelines":      "Updated HTTP guidelines.",
		"tags":            map[string]any{"mode": "append", "value": []string{"v2"}},
		"last_updated_by": "tester:http",
	}
	resp, body = r.do(http.MethodPatch, "/registry/projects/"+url.PathEscape(created.URN), patchBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch project: %d: %s", resp.StatusCode, body)
	}
	var patched struct {
		Guidelines    string                        `json:"guidelines"`
		Tags          []string                      `json:"tags"`
		FieldMetadata map[string]registry.FieldMeta `json:"field_metadata"`
	}
	if err := json.Unmarshal(body, &patched); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if patched.Guidelines != "Updated HTTP guidelines." {
		t.Errorf("patched.Guidelines = %q", patched.Guidelines)
	}
	if len(patched.Tags) != 4 {
		t.Errorf("patched.Tags = %v; want 4 tags", patched.Tags)
	}
	if meta, ok := patched.FieldMetadata["guidelines"]; !ok || meta.LastUpdatedBy != "tester:http" {
		t.Errorf("patched guidelines meta = %+v; want tester:http", meta)
	}
}

func TestRedactFieldMetadata(t *testing.T) {
	now := time.Now().UTC()
	input := map[string]registry.FieldMeta{
		"callback":     {Class: registry.FieldClassDerived, LastUpdatedBy: "sys", UpdatedAt: now},
		"host_address": {Class: registry.FieldClassDerived, LastUpdatedBy: "sys", UpdatedAt: now},
		"kind_meta":    {Class: registry.FieldClassDerived, LastUpdatedBy: "sys", UpdatedAt: now},
		"external_ids": {Class: registry.FieldClassDerived, LastUpdatedBy: "sys", UpdatedAt: now},
		"display_name": {Class: registry.FieldClassDerived, LastUpdatedBy: "user", UpdatedAt: now},
		"tags":         {Class: registry.FieldClassAuthored, LastUpdatedBy: "user", UpdatedAt: now},
		"guidelines":   {Class: registry.FieldClassAuthored, LastUpdatedBy: "user", UpdatedAt: now},
	}

	// 1. Default (no includes) — all 4 sensitive keys stripped
	defaultMeta := redactFieldMetadata(input, includeFields{})
	for _, secretKey := range []string{"callback", "host_address", "kind_meta", "external_ids"} {
		if _, ok := defaultMeta[secretKey]; ok {
			t.Errorf("defaultMeta unexpectedly contains %q", secretKey)
		}
	}
	for _, publicKey := range []string{"display_name", "tags", "guidelines"} {
		if _, ok := defaultMeta[publicKey]; !ok {
			t.Errorf("defaultMeta missing public key %q", publicKey)
		}
	}

	// 2. Selective include: callback only
	incCb := redactFieldMetadata(input, includeFields{callback: true})
	if _, ok := incCb["callback"]; !ok {
		t.Errorf("incCb missing callback")
	}
	for _, secretKey := range []string{"host_address", "kind_meta", "external_ids"} {
		if _, ok := incCb[secretKey]; ok {
			t.Errorf("incCb unexpectedly contains %q", secretKey)
		}
	}

	// 3. Selective include: host_address only
	incHost := redactFieldMetadata(input, includeFields{hostAddress: true})
	if _, ok := incHost["host_address"]; !ok {
		t.Errorf("incHost missing host_address")
	}
	if _, ok := incHost["callback"]; ok {
		t.Errorf("incHost unexpectedly contains callback")
	}

	// 4. Selective include: kind_meta only
	incMeta := redactFieldMetadata(input, includeFields{kindMeta: true})
	if _, ok := incMeta["kind_meta"]; !ok {
		t.Errorf("incMeta missing kind_meta")
	}
	if _, ok := incMeta["callback"]; ok {
		t.Errorf("incMeta unexpectedly contains callback")
	}

	// 5. Selective include: external_ids only
	incExt := redactFieldMetadata(input, includeFields{externalIDs: true})
	if _, ok := incExt["external_ids"]; !ok {
		t.Errorf("incExt missing external_ids")
	}
	if _, ok := incExt["callback"]; ok {
		t.Errorf("incExt unexpectedly contains callback")
	}

	// 6. All includes
	allMeta := redactFieldMetadata(input, includeFields{callback: true, hostAddress: true, kindMeta: true, externalIDs: true})
	if len(allMeta) != len(input) {
		t.Errorf("allMeta len = %d; want %d", len(allMeta), len(input))
	}

	// 7. Nil input returns nil
	if got := redactFieldMetadata(nil, includeFields{}); got != nil {
		t.Errorf("redactFieldMetadata(nil) = %v; want nil", got)
	}
}

func TestRegistry_Redaction_FieldMetadata(t *testing.T) {
	r := newRegServer(t)

	// Register a project with callback, host_address, kind_meta, and tags
	createBody := map[string]any{
		"display_name":    "Secretive Project",
		"description":     "Project with private operational metadata",
		"callback":        map[string]any{"scheme": "cli", "target": "/usr/local/bin/run --token=supersecret"},
		"host_address":    "10.0.0.88",
		"kind_meta":       map[string]any{"api_token": "shh-secret"},
		"tags":            []string{"internal", "confidential"},
		"guidelines":      "Keep operational coordinates secret.",
		"last_updated_by": "tester:sec",
	}

	resp, body := r.do(http.MethodPost, "/registry/projects", createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project: %d: %s", resp.StatusCode, body)
	}

	var created struct {
		URN string `json:"urn"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}

	// Confirm that internally, the row carries a "callback" entry in field_metadata
	internalProfile, err := r.svc.Lookup(context.Background(), created.URN)
	if err != nil {
		t.Fatalf("lookup internal profile: %v", err)
	}
	if cbMeta, ok := internalProfile.FieldMetadata["callback"]; !ok {
		t.Fatalf("internal profile missing field_metadata.callback entry: %+v", internalProfile.FieldMetadata)
	} else if cbMeta.Class != registry.FieldClassDerived {
		t.Errorf("internal field_metadata.callback class = %q; want derived", cbMeta.Class)
	}

	// 1. Default GET /registry/projects/{urn} must redact callback and field_metadata.callback
	respDef, bodyDef := r.do(http.MethodGet, itemURL("projects", created.URN), nil)
	if respDef.StatusCode != http.StatusOK {
		t.Fatalf("default lookup: %d: %s", respDef.StatusCode, bodyDef)
	}

	// Check string level: no "callback", "host_address", "kind_meta" keys or secret tokens
	assertNoLeakedRegistryFields(t, bodyDef)

	var defaultProfile struct {
		FieldMetadata map[string]registry.FieldMeta `json:"field_metadata"`
	}
	if err := json.Unmarshal(bodyDef, &defaultProfile); err != nil {
		t.Fatalf("unmarshal default profile: %v", err)
	}
	for _, sensitive := range []string{"callback", "host_address", "kind_meta", "external_ids"} {
		if _, ok := defaultProfile.FieldMetadata[sensitive]; ok {
			t.Errorf("default GET leaked field_metadata[%q]: %+v", sensitive, defaultProfile.FieldMetadata)
		}
	}
	for _, public := range []string{"display_name", "tags", "guidelines", "description"} {
		if _, ok := defaultProfile.FieldMetadata[public]; !ok {
			t.Errorf("default GET unexpectedly stripped public field_metadata[%q]", public)
		}
	}

	// 2. GET with ?include=callback reveals callback in top-level AND field_metadata.callback,
	// while keeping host_address and kind_meta redacted.
	respInc, bodyInc := r.do(http.MethodGet, itemURL("projects", created.URN)+"?include=callback", nil)
	if respInc.StatusCode != http.StatusOK {
		t.Fatalf("include=callback lookup: %d: %s", respInc.StatusCode, bodyInc)
	}
	var incProfile struct {
		Callback      *registry.Callback            `json:"callback"`
		HostAddress   string                        `json:"host_address"`
		FieldMetadata map[string]registry.FieldMeta `json:"field_metadata"`
	}
	if err := json.Unmarshal(bodyInc, &incProfile); err != nil {
		t.Fatalf("unmarshal include profile: %v", err)
	}
	if incProfile.Callback == nil || incProfile.Callback.Target != "/usr/local/bin/run --token=supersecret" {
		t.Errorf("include=callback did not return expected callback: %+v", incProfile.Callback)
	}
	if incProfile.HostAddress != "" {
		t.Errorf("include=callback leaked host_address: %q", incProfile.HostAddress)
	}
	if metaCb, ok := incProfile.FieldMetadata["callback"]; !ok {
		t.Errorf("include=callback did not expose field_metadata.callback")
	} else if metaCb.Class != registry.FieldClassDerived {
		t.Errorf("field_metadata.callback class = %q; want derived", metaCb.Class)
	}
	if _, ok := incProfile.FieldMetadata["host_address"]; ok {
		t.Errorf("include=callback leaked field_metadata.host_address")
	}
	if _, ok := incProfile.FieldMetadata["kind_meta"]; ok {
		t.Errorf("include=callback leaked field_metadata.kind_meta")
	}

	// 3. GET with ?full=true reveals all sensitive fields in top-level AND field_metadata.
	respFull, bodyFull := r.do(http.MethodGet, itemURL("projects", created.URN)+"?full=true", nil)
	if respFull.StatusCode != http.StatusOK {
		t.Fatalf("full=true lookup: %d: %s", respFull.StatusCode, bodyFull)
	}
	var fullProfile struct {
		Callback      *registry.Callback            `json:"callback"`
		HostAddress   string                        `json:"host_address"`
		FieldMetadata map[string]registry.FieldMeta `json:"field_metadata"`
	}
	if err := json.Unmarshal(bodyFull, &fullProfile); err != nil {
		t.Fatalf("unmarshal full profile: %v", err)
	}
	if fullProfile.Callback == nil || fullProfile.HostAddress == "" {
		t.Errorf("full=true did not return callback or host_address")
	}
	for _, sensitive := range []string{"callback", "host_address", "kind_meta"} {
		if _, ok := fullProfile.FieldMetadata[sensitive]; !ok {
			t.Errorf("full=true did not expose field_metadata[%q]", sensitive)
		}
	}
}

func TestRegistry_C3_OnboardingLookupAndTorqueSubstrate(t *testing.T) {
	r := newRegServer(t)

	// 1. Onboarding: Register a project with torque, tether, and cerberus external IDs.
	regBody := map[string]any{
		"display_name": "Tether Control Plane",
		"description":  "Local agent session control plane",
		"external_ids": []map[string]any{
			{"substrate": "tether", "external_id": "tether"},
			{"substrate": "torque", "external_id": "PRJ-TETHER-01"},
			{"substrate": "cerberus", "external_id": "tether-runtime"},
		},
		"guidelines": "Always verify with make check",
		"tags":       []string{"runtime", "daemon"},
		"callback": map[string]any{
			"scheme": "cli",
			"target": "mux describe --json",
		},
	}
	resp, body := r.do(http.MethodPost, "/registry/projects", regBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d: %s", resp.StatusCode, body)
	}
	var created registry.Profile
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal created profile: %v", err)
	}
	if created.URN == "" {
		t.Fatal("expected non-empty URN minted")
	}

	// 2. Resolve query:
	// 2a. Default GET redacts external_ids
	respDef, bodyDef := r.do(http.MethodGet, itemURL("projects", created.URN), nil)
	if respDef.StatusCode != http.StatusOK {
		t.Fatalf("default lookup status = %d: %s", respDef.StatusCode, bodyDef)
	}
	var defProfile registry.Profile
	if err := json.Unmarshal(bodyDef, &defProfile); err != nil {
		t.Fatalf("unmarshal default profile: %v", err)
	}
	if len(defProfile.ExternalIDs) != 0 {
		t.Errorf("default GET leaked external_ids: %+v", defProfile.ExternalIDs)
	}

	// 2b. GET with ?include=external_ids reveals all attached external IDs
	respInc, bodyInc := r.do(http.MethodGet, itemURL("projects", created.URN)+"?include=external_ids", nil)
	if respInc.StatusCode != http.StatusOK {
		t.Fatalf("include=external_ids lookup status = %d: %s", respInc.StatusCode, bodyInc)
	}
	var incProfile registry.Profile
	if err := json.Unmarshal(bodyInc, &incProfile); err != nil {
		t.Fatalf("unmarshal include profile: %v", err)
	}
	if len(incProfile.ExternalIDs) != 3 {
		t.Fatalf("external_ids count = %d, want 3; got %+v", len(incProfile.ExternalIDs), incProfile.ExternalIDs)
	}
	extMap := map[string]string{}
	for _, ext := range incProfile.ExternalIDs {
		extMap[ext.Substrate] = ext.ExternalID
	}
	if extMap["torque"] != "PRJ-TETHER-01" {
		t.Errorf("torque external_id = %q, want PRJ-TETHER-01", extMap["torque"])
	}
	if extMap["tether"] != "tether" {
		t.Errorf("tether external_id = %q, want tether", extMap["tether"])
	}
	if extMap["cerberus"] != "tether-runtime" {
		t.Errorf("cerberus external_id = %q, want tether-runtime", extMap["cerberus"])
	}

	// 3. Reverse query (HTTP):
	// Given substrate "torque" and external_id "PRJ-TETHER-01", resolve to project
	respRev, bodyRev := r.do(http.MethodGet, "/registry/projects?external_id=PRJ-TETHER-01&substrate=torque", nil)
	if respRev.StatusCode != http.StatusOK {
		t.Fatalf("reverse lookup status = %d: %s", respRev.StatusCode, bodyRev)
	}
	var revEnv map[string]registry.Profile
	if err := json.Unmarshal(bodyRev, &revEnv); err != nil {
		t.Fatalf("unmarshal reverse lookup: %v", err)
	}
	revProj, ok := revEnv["project"]
	if !ok {
		t.Fatalf("reverse lookup envelope missing 'project' key: %s", bodyRev)
	}
	if revProj.URN != created.URN {
		t.Errorf("reverse lookup URN = %q, want %q", revProj.URN, created.URN)
	}
	if revProj.DisplayName != "Tether Control Plane" {
		t.Errorf("reverse lookup display_name = %q, want Tether Control Plane", revProj.DisplayName)
	}

	// 4. Reverse query with selective include and full via HTTP:
	// 4a. Reverse lookup with ?include=external_ids reveals external_ids
	respRevInc, bodyRevInc := r.do(http.MethodGet, "/registry/projects?external_id=PRJ-TETHER-01&substrate=torque&include=external_ids", nil)
	if respRevInc.StatusCode != http.StatusOK {
		t.Fatalf("reverse lookup include status = %d: %s", respRevInc.StatusCode, bodyRevInc)
	}
	var revIncEnv map[string]registry.Profile
	if err := json.Unmarshal(bodyRevInc, &revIncEnv); err != nil {
		t.Fatalf("unmarshal reverse include: %v", err)
	}
	if len(revIncEnv["project"].ExternalIDs) != 3 {
		t.Errorf("reverse include external_ids count = %d, want 3", len(revIncEnv["project"].ExternalIDs))
	}

	// 4b. Reverse lookup with ?full=true reveals callback and external_ids
	respRevFull, bodyRevFull := r.do(http.MethodGet, "/registry/projects?external_id=PRJ-TETHER-01&substrate=torque&full=true", nil)
	if respRevFull.StatusCode != http.StatusOK {
		t.Fatalf("reverse lookup full status = %d: %s", respRevFull.StatusCode, bodyRevFull)
	}
	var revFullEnv map[string]registry.Profile
	if err := json.Unmarshal(bodyRevFull, &revFullEnv); err != nil {
		t.Fatalf("unmarshal reverse full: %v", err)
	}
	revFullProj := revFullEnv["project"]
	if revFullProj.Callback == nil || revFullProj.Callback.Target != "mux describe --json" {
		t.Errorf("reverse full did not return callback: %+v", revFullProj.Callback)
	}
	if len(revFullProj.ExternalIDs) != 3 {
		t.Errorf("reverse full external_ids count = %d, want 3", len(revFullProj.ExternalIDs))
	}

	// 5. Partial-coverage discipline:
	// Register Project Partial with only tether substrate ID (no torque)
	respPart, bodyPart := r.do(http.MethodPost, "/registry/projects", map[string]any{
		"display_name": "Partial Coverage Project",
		"external_ids": []map[string]any{
			{"substrate": "tether", "external_id": "partial-prj"},
		},
	})
	if respPart.StatusCode != http.StatusCreated {
		t.Fatalf("register partial status = %d: %s", respPart.StatusCode, bodyPart)
	}
	var partialProj registry.Profile
	if err := json.Unmarshal(bodyPart, &partialProj); err != nil {
		t.Fatalf("unmarshal partial profile: %v", err)
	}

	// 5a. Querying with ?include=external_ids returns an ordinary answer with 1 external ID,
	// omitting torque without error.
	respPartInc, bodyPartInc := r.do(http.MethodGet, itemURL("projects", partialProj.URN)+"?include=external_ids", nil)
	if respPartInc.StatusCode != http.StatusOK {
		t.Fatalf("partial include status = %d: %s", respPartInc.StatusCode, bodyPartInc)
	}
	var partialInc registry.Profile
	if err := json.Unmarshal(bodyPartInc, &partialInc); err != nil {
		t.Fatalf("unmarshal partial include: %v", err)
	}
	if len(partialInc.ExternalIDs) != 1 || partialInc.ExternalIDs[0].Substrate != "tether" {
		t.Errorf("partial external_ids = %+v, want only tether", partialInc.ExternalIDs)
	}

	// 5b. Reverse lookup on torque for non-existent ID returns 404 Not Found cleanly as an ordinary response
	respMissing, bodyMissing := r.do(http.MethodGet, "/registry/projects?external_id=PRJ-NONEXISTENT&substrate=torque", nil)
	if respMissing.StatusCode != http.StatusNotFound {
		t.Errorf("reverse lookup for missing torque ID status = %d, want 404; body=%s", respMissing.StatusCode, bodyMissing)
	}

	// 6. Offboarding Terminal States:
	// 6a. Deregister soft-deletes to status="deprecated". Row remains resolvable by URN.
	respDel, bodyDel := r.do(http.MethodDelete, itemURL("projects", partialProj.URN), nil)
	if respDel.StatusCode != http.StatusOK {
		t.Fatalf("deregister status = %d: %s", respDel.StatusCode, bodyDel)
	}
	var deregProfile registry.Profile
	if err := json.Unmarshal(bodyDel, &deregProfile); err != nil {
		t.Fatalf("unmarshal deregister profile: %v", err)
	}
	if deregProfile.Status != registry.StatusDeprecated {
		t.Errorf("deregister status = %q, want deprecated", deregProfile.Status)
	}

	// Direct URN lookup returns the deprecated profile
	respDeregLook, bodyDeregLook := r.do(http.MethodGet, itemURL("projects", partialProj.URN), nil)
	if respDeregLook.StatusCode != http.StatusOK {
		t.Fatalf("direct lookup of deprecated status = %d: %s", respDeregLook.StatusCode, bodyDeregLook)
	}
	var checkDereg registry.Profile
	if err := json.Unmarshal(bodyDeregLook, &checkDereg); err != nil {
		t.Fatalf("unmarshal deprecated check: %v", err)
	}
	if checkDereg.Status != registry.StatusDeprecated {
		t.Errorf("status = %q, want deprecated", checkDereg.Status)
	}

	// 6b. Merge: duplicate project gets merged into destination.
	// Register duplicate project Dup
	respDup, bodyDup := r.do(http.MethodPost, "/registry/projects", map[string]any{
		"display_name": "Duplicate Project",
		"external_ids": []map[string]any{
			{"substrate": "torque", "external_id": "PRJ-DUP-99"},
		},
	})
	if respDup.StatusCode != http.StatusCreated {
		t.Fatalf("register dup status = %d: %s", respDup.StatusCode, bodyDup)
	}
	var dupProj registry.Profile
	if err := json.Unmarshal(bodyDup, &dupProj); err != nil {
		t.Fatalf("unmarshal dup: %v", err)
	}

	// Target project Dest
	respDest, bodyDest := r.do(http.MethodPost, "/registry/projects", map[string]any{
		"display_name": "Canonical Destination Project",
		"external_ids": []map[string]any{
			{"substrate": "tether", "external_id": "canonical-dest"},
		},
	})
	if respDest.StatusCode != http.StatusCreated {
		t.Fatalf("register dest status = %d: %s", respDest.StatusCode, bodyDest)
	}
	var destProj registry.Profile
	if err := json.Unmarshal(bodyDest, &destProj); err != nil {
		t.Fatalf("unmarshal dest: %v", err)
	}

	// Merge Dup into Dest
	respMerge, bodyMerge := r.do(http.MethodPost, itemURL("projects", dupProj.URN)+"/merge", map[string]any{
		"into": destProj.URN,
	})
	if respMerge.StatusCode != http.StatusOK {
		t.Fatalf("merge status = %d: %s", respMerge.StatusCode, bodyMerge)
	}

	// Verify Dup is now deprecated with merged_into pointing to Dest
	respDupAfter, bodyDupAfter := r.do(http.MethodGet, itemURL("projects", dupProj.URN), nil)
	if respDupAfter.StatusCode != http.StatusOK {
		t.Fatalf("lookup dup after merge: %d: %s", respDupAfter.StatusCode, bodyDupAfter)
	}
	var dupAfter registry.Profile
	if err := json.Unmarshal(bodyDupAfter, &dupAfter); err != nil {
		t.Fatalf("unmarshal dupAfter: %v", err)
	}
	if dupAfter.Status != registry.StatusMerged {
		t.Errorf("dup status = %q, want merged", dupAfter.Status)
	}
	if dupAfter.MergedInto != destProj.URN {
		t.Errorf("dup merged_into = %q, want %q", dupAfter.MergedInto, destProj.URN)
	}

	// Reverse lookup on "PRJ-DUP-99" (substrate torque) now resolves to Dest
	respRevMerged, bodyRevMerged := r.do(http.MethodGet, "/registry/projects?external_id=PRJ-DUP-99&substrate=torque", nil)
	if respRevMerged.StatusCode != http.StatusOK {
		t.Fatalf("reverse lookup after merge status = %d: %s", respRevMerged.StatusCode, bodyRevMerged)
	}
	var revMergedEnv map[string]registry.Profile
	if err := json.Unmarshal(bodyRevMerged, &revMergedEnv); err != nil {
		t.Fatalf("unmarshal revMergedEnv: %v", err)
	}
	if revMergedEnv["project"].URN != destProj.URN {
		t.Errorf("reverse lookup after merge resolved to %q, want %q", revMergedEnv["project"].URN, destProj.URN)
	}
}
