package settings

import (
	"reflect"
	"testing"
)

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}

func TestResolveEffectiveOnboarding_Precedence(t *testing.T) {
	global := &OnboardingSettings{
		RequiredProps:    []string{"docs_url", "owner"},
		MCPOptInOffered:  boolPtr(false),
		DefaultLLMPolicy: "claude-3-haiku",
		Custom: map[string]string{
			"env":    "production",
			"region": "us-east-1",
		},
	}

	project := &OnboardingSettings{
		RequiredProps:    []string{"docs_url", "project_root"},
		MCPOptInOffered:  boolPtr(true),
		DefaultLLMPolicy: "claude-3-5-sonnet",
		Custom: map[string]string{
			"region": "us-west-2",
			"tier":   "standard",
		},
	}

	user := &OnboardingSettings{
		RequiredProps:    []string{"docs_url", "custom_user_prop"},
		MCPOptInOffered:  boolPtr(false),
		DefaultLLMPolicy: "claude-3-opus",
		Custom: map[string]string{
			"tier": "vip",
		},
	}

	// 1. All tiers present: User wins on scalar and slices, maps overlay.
	eff := ResolveEffectiveOnboarding(global, project, user)
	if !reflect.DeepEqual(eff.RequiredProps, []string{"docs_url", "custom_user_prop"}) {
		t.Errorf("RequiredProps = %v, want user's props", eff.RequiredProps)
	}
	if eff.MCPOptInOffered == nil || *eff.MCPOptInOffered != false {
		t.Errorf("MCPOptInOffered = %v, want false (user)", eff.MCPOptInOffered)
	}
	if eff.DefaultLLMPolicy != "claude-3-opus" {
		t.Errorf("DefaultLLMPolicy = %q, want user's policy", eff.DefaultLLMPolicy)
	}
	wantCustom := map[string]string{
		"env":    "production", // from global
		"region": "us-west-2",  // from project
		"tier":   "vip",        // from user
	}
	if !reflect.DeepEqual(eff.Custom, wantCustom) {
		t.Errorf("Custom = %v, want %v", eff.Custom, wantCustom)
	}

	// 2. User omitted: Project wins.
	effProj := ResolveEffectiveOnboarding(global, project, nil)
	if !reflect.DeepEqual(effProj.RequiredProps, []string{"docs_url", "project_root"}) {
		t.Errorf("RequiredProps = %v, want project's props", effProj.RequiredProps)
	}
	if effProj.MCPOptInOffered == nil || *effProj.MCPOptInOffered != true {
		t.Errorf("MCPOptInOffered = %v, want true (project)", effProj.MCPOptInOffered)
	}
	if effProj.DefaultLLMPolicy != "claude-3-5-sonnet" {
		t.Errorf("DefaultLLMPolicy = %q, want project's policy", effProj.DefaultLLMPolicy)
	}
	wantCustomProj := map[string]string{
		"env":    "production", // from global
		"region": "us-west-2",  // from project
		"tier":   "standard",   // from project
	}
	if !reflect.DeepEqual(effProj.Custom, wantCustomProj) {
		t.Errorf("Custom = %v, want %v", effProj.Custom, wantCustomProj)
	}

	// 3. User & Project omitted: Global wins.
	effGlobal := ResolveEffectiveOnboarding(global, nil, nil)
	if !reflect.DeepEqual(effGlobal.RequiredProps, []string{"docs_url", "owner"}) {
		t.Errorf("RequiredProps = %v, want global's props", effGlobal.RequiredProps)
	}
	if effGlobal.MCPOptInOffered == nil || *effGlobal.MCPOptInOffered != false {
		t.Errorf("MCPOptInOffered = %v, want false (global)", effGlobal.MCPOptInOffered)
	}
	if effGlobal.DefaultLLMPolicy != "claude-3-haiku" {
		t.Errorf("DefaultLLMPolicy = %q, want global's policy", effGlobal.DefaultLLMPolicy)
	}
	if !reflect.DeepEqual(effGlobal.Custom, global.Custom) {
		t.Errorf("Custom = %v, want %v", effGlobal.Custom, global.Custom)
	}

	// 4. All nil: returns zero/default structure.
	effEmpty := ResolveEffectiveOnboarding(nil, nil, nil)
	if len(effEmpty.RequiredProps) != 0 {
		t.Errorf("RequiredProps = %v, want empty", effEmpty.RequiredProps)
	}
	if effEmpty.MCPOptInOffered != nil {
		t.Errorf("MCPOptInOffered = %v, want nil", effEmpty.MCPOptInOffered)
	}
	if effEmpty.DefaultLLMPolicy != "" {
		t.Errorf("DefaultLLMPolicy = %q, want empty", effEmpty.DefaultLLMPolicy)
	}
}

func TestEffectiveValue(t *testing.T) {
	global := stringPtr("global-val")
	project := stringPtr("project-val")
	user := stringPtr("user-val")
	fallback := "fallback-val"

	if got := EffectiveValue(global, project, user, fallback); got != "user-val" {
		t.Errorf("got %q, want user-val", got)
	}
	if got := EffectiveValue(global, project, nil, fallback); got != "project-val" {
		t.Errorf("got %q, want project-val", got)
	}
	if got := EffectiveValue(global, nil, nil, fallback); got != "global-val" {
		t.Errorf("got %q, want global-val", got)
	}
	if got := EffectiveValue(nil, nil, nil, fallback); got != "fallback-val" {
		t.Errorf("got %q, want fallback-val", got)
	}
	empty := stringPtr("")
	if got := EffectiveValue(global, empty, empty, fallback); got != "global-val" {
		t.Errorf("got %q, want global-val (empty strings fall through)", got)
	}
}
