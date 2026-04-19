package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/chrispian/agent-mux/internal/config"
)

// fakeCatalogLoader is a struct-style CatalogLoader stub. Tests either
// set cat (success path) or err (failure path); never both.
type fakeCatalogLoader struct {
	cat *config.Catalog
	err error
}

func (f *fakeCatalogLoader) Load() (*config.Catalog, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cat, nil
}

func newCatalogHandler(loader CatalogLoader) http.Handler {
	return NewHandler(Deps{Catalog: loader})
}

func TestHandleListProjects_Success(t *testing.T) {
	loader := &fakeCatalogLoader{cat: &config.Catalog{
		Projects: map[string]config.Project{
			"p2": {ID: "p2", Name: "Beta"},
			"p1": {ID: "p1", Name: "Alpha"},
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/catalog/projects", nil)
	rr := httptest.NewRecorder()
	newCatalogHandler(loader).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var res ListProjectsResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(res.Projects))
	}
	// Deterministic order: sort by ID so TUI doesn't re-render churn.
	if res.Projects[0].ID != "p1" || res.Projects[1].ID != "p2" {
		t.Errorf("projects order = [%q, %q], want [p1, p2]", res.Projects[0].ID, res.Projects[1].ID)
	}
}

func TestHandleListProjects_Empty(t *testing.T) {
	loader := &fakeCatalogLoader{cat: &config.Catalog{Projects: map[string]config.Project{}}}
	req := httptest.NewRequest(http.MethodGet, "/catalog/projects", nil)
	rr := httptest.NewRecorder()
	newCatalogHandler(loader).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	// Empty response must still carry the key with [] (not null) so TUI
	// clients can range without nil-checking.
	body := rr.Body.String()
	if body == `{"projects":null}`+"\n" {
		t.Errorf("empty catalog serialized as null: %s", body)
	}
}

func TestHandleListAgents_Success(t *testing.T) {
	loader := &fakeCatalogLoader{cat: &config.Catalog{
		Agents: map[string]config.Agent{
			"a1": {ID: "a1", Name: "Alpha", Roles: []string{"general"}},
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/catalog/agents", nil)
	rr := httptest.NewRecorder()
	newCatalogHandler(loader).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var res ListAgentsResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Agents) != 1 || res.Agents[0].ID != "a1" {
		t.Errorf("agents = %+v", res.Agents)
	}
}

func TestHandleListProviders_Success(t *testing.T) {
	loader := &fakeCatalogLoader{cat: &config.Catalog{
		Providers: map[string]config.Provider{
			"stub": {ID: "stub", Type: "api", Command: ""},
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/catalog/providers", nil)
	rr := httptest.NewRecorder()
	newCatalogHandler(loader).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var res ListProvidersResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Providers) != 1 || res.Providers[0].ID != "stub" {
		t.Errorf("providers = %+v", res.Providers)
	}
}

func TestHandleListLaunches_Success(t *testing.T) {
	loader := &fakeCatalogLoader{cat: &config.Catalog{
		Launches: map[string]config.Launch{
			"demo-launch": {ID: "demo-launch", Project: "demo", Agent: "demo-agent", Provider: "claude-code"},
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/catalog/launches", nil)
	rr := httptest.NewRecorder()
	newCatalogHandler(loader).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var res ListLaunchesResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Launches) != 1 || res.Launches[0].ID != "demo-launch" {
		t.Errorf("launches = %+v", res.Launches)
	}
}

func TestHandleCatalog_MethodNotAllowed(t *testing.T) {
	loader := &fakeCatalogLoader{cat: &config.Catalog{}}
	h := newCatalogHandler(loader)
	routes := []string{
		"/catalog/projects",
		"/catalog/agents",
		"/catalog/providers",
		"/catalog/launches",
	}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	for _, route := range routes {
		for _, m := range methods {
			req := httptest.NewRequest(m, route, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s status = %d, want 405", m, route, rr.Code)
				continue
			}
			env := decodeErr(t, rr)
			if env.Error.Code != CodeMethodNotAllowed {
				t.Errorf("%s %s code = %q, want %q", m, route, env.Error.Code, CodeMethodNotAllowed)
			}
		}
	}
}

func TestHandleCatalog_LoaderError(t *testing.T) {
	// Mimics a malformed YAML file scenario: config.Load wraps with path
	// info ("parse /path/to/file.yaml: ..."). The handler forwards the
	// loader error verbatim in the envelope message.
	loader := &fakeCatalogLoader{err: errors.New("parse /tmp/catalog/launches/bad.yaml: yaml: line 2: mapping values")}
	h := newCatalogHandler(loader)

	for _, route := range []string{"/catalog/projects", "/catalog/agents", "/catalog/providers", "/catalog/launches"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("%s status = %d, want 500", route, rr.Code)
			continue
		}
		env := decodeErr(t, rr)
		if env.Error.Code != CodeInternalError {
			t.Errorf("%s code = %q, want %q", route, env.Error.Code, CodeInternalError)
		}
		// The error message must cite the offending path so an operator
		// knows which file to fix.
		if msg := env.Error.Message; msg == "" || !containsPath(msg, "/tmp/catalog/launches/bad.yaml") {
			t.Errorf("%s message %q missing offending path", route, msg)
		}
	}
}

func TestHandleCatalog_LoaderReturnsNil(t *testing.T) {
	// A misbehaving loader implementation that returns (nil, nil) gets
	// caught by the guard in loadCatalog rather than NPE-ing on nil maps.
	loader := &fakeCatalogLoader{}
	req := httptest.NewRequest(http.MethodGet, "/catalog/projects", nil)
	rr := httptest.NewRecorder()
	newCatalogHandler(loader).ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

func TestCatalogRoutes_NotRegisteredWithoutLoader(t *testing.T) {
	// Nil Catalog in Deps means /catalog/* isn't mounted; default 404.
	h := NewHandler(Deps{})
	for _, route := range []string{"/catalog/projects", "/catalog/agents", "/catalog/providers", "/catalog/launches"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", route, rr.Code)
		}
	}
}

// TestHandleCatalog_FixtureIntegration exercises the full handler →
// realistic CatalogLoader path against the checked-in examples/catalog
// fixture. Catches breakage at the boundary between config.Load's
// YAML parsing and the handler's response shape.
func TestHandleCatalog_FixtureIntegration(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	catalogRoot := filepath.Join(filepath.Dir(file), "..", "..", "examples", "catalog")

	loader := fsLoaderFunc(func() (*config.Catalog, error) {
		return config.Load(catalogRoot)
	})
	h := NewHandler(Deps{Catalog: loader})

	// Projects
	req := httptest.NewRequest(http.MethodGet, "/catalog/projects", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("projects: status = %d: %s", rr.Code, rr.Body.String())
	}
	var pr ListProjectsResponse
	if err := json.NewDecoder(rr.Body).Decode(&pr); err != nil {
		t.Fatalf("projects decode: %v", err)
	}
	if len(pr.Projects) == 0 {
		t.Error("projects: expected at least the demo fixture")
	}

	// Launches — demo-launch + api-stub-launch per examples/catalog/launches/.
	req = httptest.NewRequest(http.MethodGet, "/catalog/launches", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("launches: status = %d: %s", rr.Code, rr.Body.String())
	}
	var lr ListLaunchesResponse
	if err := json.NewDecoder(rr.Body).Decode(&lr); err != nil {
		t.Fatalf("launches decode: %v", err)
	}
	var ids []string
	for _, l := range lr.Launches {
		ids = append(ids, l.ID)
	}
	if !contains(ids, "demo-launch") {
		t.Errorf("launches: missing demo-launch; got %v", ids)
	}
}

// fsLoaderFunc is a function-table CatalogLoader for the fixture test.
type fsLoaderFunc func() (*config.Catalog, error)

func (f fsLoaderFunc) Load() (*config.Catalog, error) { return f() }

func containsPath(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
