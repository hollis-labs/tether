package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/settings"
	"github.com/hollis-labs/tether/internal/store"
)

func newSettingsServer(t *testing.T) (*settings.Service, http.Handler) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	st := settings.NewStorage(db)
	svc := settings.NewService(st)
	handler := api.NewHandler(api.Deps{Settings: svc})
	return svc, handler
}

func boolPtr(b bool) *bool {
	return &b
}

func TestSettings_EffectiveOnboarding_Cascade(t *testing.T) {
	svc, h := newSettingsServer(t)
	ctx := context.Background()

	// Seed global
	if err := svc.SetOnboarding(ctx, settings.ScopeGlobal, "", settings.OnboardingSettings{
		RequiredProps:    []string{"docs_url"},
		MCPOptInOffered:  boolPtr(false),
		DefaultLLMPolicy: "claude-3-haiku",
		Custom:           map[string]string{"env": "prod"},
	}); err != nil {
		t.Fatalf("SetOnboarding global: %v", err)
	}

	// Seed project
	projectID := "msg://project/project-mux/prj_alpha"
	if err := svc.SetOnboarding(ctx, settings.ScopeProject, projectID, settings.OnboardingSettings{
		RequiredProps:    []string{"docs_url", "project_root"},
		MCPOptInOffered:  boolPtr(true),
		DefaultLLMPolicy: "claude-3-5-sonnet",
	}); err != nil {
		t.Fatalf("SetOnboarding project: %v", err)
	}

	// Seed user
	userID := "msg://agent/agent-mux/usr_chrispian"
	if err := svc.SetOnboarding(ctx, settings.ScopeUser, userID, settings.OnboardingSettings{
		MCPOptInOffered: boolPtr(false),
		Custom:          map[string]string{"env": "staging"},
	}); err != nil {
		t.Fatalf("SetOnboarding user: %v", err)
	}

	// 1. Query with project and user
	req := httptest.NewRequest(http.MethodGet, "/settings/onboarding?project="+projectID+"&user="+userID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got settings.OnboardingSettings
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !reflect.DeepEqual(got.RequiredProps, []string{"docs_url", "project_root"}) {
		t.Errorf("RequiredProps = %v, want [docs_url project_root]", got.RequiredProps)
	}
	if got.MCPOptInOffered == nil || *got.MCPOptInOffered != false {
		t.Errorf("MCPOptInOffered = %v, want false (user)", got.MCPOptInOffered)
	}
	if got.DefaultLLMPolicy != "claude-3-5-sonnet" {
		t.Errorf("DefaultLLMPolicy = %q, want claude-3-5-sonnet", got.DefaultLLMPolicy)
	}
	if got.Custom["env"] != "staging" {
		t.Errorf("Custom[env] = %q, want staging (user overlay)", got.Custom["env"])
	}
}

func TestSettings_ScopedOnboarding_CRUD(t *testing.T) {
	_, h := newSettingsServer(t)

	// 1. PUT global onboarding
	putBody := settings.OnboardingSettings{
		RequiredProps:   []string{"docs_url"},
		MCPOptInOffered: boolPtr(true),
	}
	b, _ := json.Marshal(putBody)
	req := httptest.NewRequest(http.MethodPut, "/settings/onboarding/global", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT global status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	// 2. GET global onboarding
	req = httptest.NewRequest(http.MethodGet, "/settings/onboarding/global", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET global status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got settings.OnboardingSettings
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if !reflect.DeepEqual(got.RequiredProps, []string{"docs_url"}) {
		t.Errorf("RequiredProps = %v, want [docs_url]", got.RequiredProps)
	}
	if got.MCPOptInOffered == nil || *got.MCPOptInOffered != true {
		t.Errorf("MCPOptInOffered = %v, want true", got.MCPOptInOffered)
	}

	// 3. GET project before setting -> 404
	req = httptest.NewRequest(http.MethodGet, "/settings/onboarding/project?scope_id=prj_foo", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET non-existent project status = %d, want 404", rec.Code)
	}
}

func TestSettings_ValidationErrors(t *testing.T) {
	_, h := newSettingsServer(t)

	// Global with non-empty scope_id -> 400
	req := httptest.NewRequest(http.MethodGet, "/settings/onboarding/global?scope_id=forbidden", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("global with scope_id status = %d, want 400", rec.Code)
	}

	// Project without scope_id -> 400
	req = httptest.NewRequest(http.MethodGet, "/settings/onboarding/project", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("project without scope_id status = %d, want 400", rec.Code)
	}

	// Invalid scope -> 400
	req = httptest.NewRequest(http.MethodGet, "/settings/onboarding/unknown", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown scope status = %d, want 400", rec.Code)
	}

	// Invalid method on /settings/onboarding -> 405
	req = httptest.NewRequest(http.MethodPost, "/settings/onboarding", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /settings/onboarding status = %d, want 405", rec.Code)
	}
}
