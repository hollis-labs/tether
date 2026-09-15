package settings_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hollis-labs/tether/internal/settings"
	"github.com/hollis-labs/tether/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db
}

func boolPtr(b bool) *bool {
	return &b
}

func TestStorage_CRUD(t *testing.T) {
	db := openTestDB(t)
	s := settings.NewStorage(db)
	ctx := context.Background()

	// 1. Set a setting
	err := s.Set(ctx, settings.Setting{
		Scope:     settings.ScopeGlobal,
		ScopeID:   "",
		Key:       "theme",
		ValueJSON: `"dark"`,
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	// 2. Get the setting
	got, err := s.Get(ctx, settings.ScopeGlobal, "", "theme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ValueJSON != `"dark"` {
		t.Errorf("ValueJSON = %q, want %q", got.ValueJSON, `"dark"`)
	}

	// 3. Upsert / update the setting
	err = s.Set(ctx, settings.Setting{
		Scope:     settings.ScopeGlobal,
		ScopeID:   "",
		Key:       "theme",
		ValueJSON: `"light"`,
	})
	if err != nil {
		t.Fatalf("Set update: %v", err)
	}
	got, err = s.Get(ctx, settings.ScopeGlobal, "", "theme")
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.ValueJSON != `"light"` {
		t.Errorf("ValueJSON = %q, want %q", got.ValueJSON, `"light"`)
	}

	// 4. List by scope
	err = s.Set(ctx, settings.Setting{
		Scope:     settings.ScopeGlobal,
		ScopeID:   "",
		Key:       "accent",
		ValueJSON: `"cyan"`,
	})
	if err != nil {
		t.Fatalf("Set second key: %v", err)
	}
	list, err := s.ListByScope(ctx, settings.ScopeGlobal, "")
	if err != nil {
		t.Fatalf("ListByScope: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len(list) = %d, want 2", len(list))
	}

	// 5. Delete
	if err := s.Delete(ctx, settings.ScopeGlobal, "", "accent"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = s.Get(ctx, settings.ScopeGlobal, "", "accent")
	if err == nil {
		t.Errorf("expected error after delete, got nil")
	}
}

func TestStorage_ResolveEffectiveOnboarding(t *testing.T) {
	db := openTestDB(t)
	s := settings.NewStorage(db)
	ctx := context.Background()

	// 1. Set Global onboarding config
	err := s.SetOnboarding(ctx, settings.ScopeGlobal, "", settings.OnboardingSettings{
		RequiredProps:    []string{"docs_url"},
		MCPOptInOffered:  boolPtr(false),
		DefaultLLMPolicy: "claude-3-haiku",
		Custom: map[string]string{
			"env": "fleet",
		},
	})
	if err != nil {
		t.Fatalf("SetOnboarding global: %v", err)
	}

	// Resolve with no project or user: global values returned
	eff, err := s.ResolveEffectiveOnboarding(ctx, "", "")
	if err != nil {
		t.Fatalf("ResolveEffectiveOnboarding (global only): %v", err)
	}
	if !reflect.DeepEqual(eff.RequiredProps, []string{"docs_url"}) {
		t.Errorf("RequiredProps = %v, want [docs_url]", eff.RequiredProps)
	}
	if eff.MCPOptInOffered == nil || *eff.MCPOptInOffered != false {
		t.Errorf("MCPOptInOffered = %v, want false", eff.MCPOptInOffered)
	}
	if eff.DefaultLLMPolicy != "claude-3-haiku" {
		t.Errorf("DefaultLLMPolicy = %q, want claude-3-haiku", eff.DefaultLLMPolicy)
	}

	// 2. Set Project onboarding config
	projectID := "msg://project/project-mux/prj_001"
	err = s.SetOnboarding(ctx, settings.ScopeProject, projectID, settings.OnboardingSettings{
		RequiredProps:    []string{"docs_url", "project_root"},
		MCPOptInOffered:  boolPtr(true),
		DefaultLLMPolicy: "claude-3-5-sonnet",
		Custom: map[string]string{
			"team": "core-infra",
		},
	})
	if err != nil {
		t.Fatalf("SetOnboarding project: %v", err)
	}

	// Resolve with project: project overrides global
	effProj, err := s.ResolveEffectiveOnboarding(ctx, projectID, "")
	if err != nil {
		t.Fatalf("ResolveEffectiveOnboarding (project): %v", err)
	}
	if !reflect.DeepEqual(effProj.RequiredProps, []string{"docs_url", "project_root"}) {
		t.Errorf("RequiredProps = %v, want project props", effProj.RequiredProps)
	}
	if effProj.MCPOptInOffered == nil || *effProj.MCPOptInOffered != true {
		t.Errorf("MCPOptInOffered = %v, want true", effProj.MCPOptInOffered)
	}
	if effProj.DefaultLLMPolicy != "claude-3-5-sonnet" {
		t.Errorf("DefaultLLMPolicy = %q, want claude-3-5-sonnet", effProj.DefaultLLMPolicy)
	}
	wantCustomProj := map[string]string{
		"env":  "fleet",
		"team": "core-infra",
	}
	if !reflect.DeepEqual(effProj.Custom, wantCustomProj) {
		t.Errorf("Custom = %v, want %v", effProj.Custom, wantCustomProj)
	}

	// 3. Set User onboarding config
	userID := "msg://agent/agent-mux/usr_chrispian"
	err = s.SetOnboarding(ctx, settings.ScopeUser, userID, settings.OnboardingSettings{
		MCPOptInOffered: boolPtr(false),
		Custom: map[string]string{
			"team": "custom-lead",
		},
	})
	if err != nil {
		t.Fatalf("SetOnboarding user: %v", err)
	}

	// Resolve with both project and user: user overrides project where set, falls through where not
	effUser, err := s.ResolveEffectiveOnboarding(ctx, projectID, userID)
	if err != nil {
		t.Fatalf("ResolveEffectiveOnboarding (user): %v", err)
	}
	// User didn't specify RequiredProps -> falls through to Project
	if !reflect.DeepEqual(effUser.RequiredProps, []string{"docs_url", "project_root"}) {
		t.Errorf("RequiredProps = %v, want project props", effUser.RequiredProps)
	}
	// User specified MCPOptInOffered -> user wins (false)
	if effUser.MCPOptInOffered == nil || *effUser.MCPOptInOffered != false {
		t.Errorf("MCPOptInOffered = %v, want false (user override)", effUser.MCPOptInOffered)
	}
	// User didn't specify DefaultLLMPolicy -> falls through to Project
	if effUser.DefaultLLMPolicy != "claude-3-5-sonnet" {
		t.Errorf("DefaultLLMPolicy = %q, want claude-3-5-sonnet (project)", effUser.DefaultLLMPolicy)
	}
	wantCustomUser := map[string]string{
		"env":  "fleet",
		"team": "custom-lead", // user overrides project
	}
	if !reflect.DeepEqual(effUser.Custom, wantCustomUser) {
		t.Errorf("Custom = %v, want %v", effUser.Custom, wantCustomUser)
	}
}
