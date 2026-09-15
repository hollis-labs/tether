package client

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/settings"
)

func boolPtr(b bool) *bool {
	return &b
}

func TestSettingsClient_GetEffectiveOnboarding(t *testing.T) {
	want := settings.OnboardingSettings{
		RequiredProps:    []string{"docs_url", "project_root"},
		MCPOptInOffered:  boolPtr(true),
		DefaultLLMPolicy: "claude-3-5-sonnet",
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/settings/onboarding" {
			t.Errorf("path = %q, want /settings/onboarding", r.URL.Path)
		}
		if r.URL.Query().Get("project") != "prj_foo" {
			t.Errorf("project query = %q, want prj_foo", r.URL.Query().Get("project"))
		}
		if r.URL.Query().Get("user") != "usr_bar" {
			t.Errorf("user query = %q, want usr_bar", r.URL.Query().Get("user"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	})

	c := newRegistryClient(t, h)
	got, err := c.Settings().GetEffectiveOnboarding(context.Background(), "prj_foo", "usr_bar")
	if err != nil {
		t.Fatalf("GetEffectiveOnboarding: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSettingsClient_ScopedOnboarding_RoundTrip(t *testing.T) {
	var saved settings.OnboardingSettings

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/settings/onboarding/project" {
			t.Errorf("path = %q, want /settings/onboarding/project", r.URL.Path)
		}
		if r.URL.Query().Get("scope_id") != "prj_foo" {
			t.Errorf("scope_id = %q, want prj_foo", r.URL.Query().Get("scope_id"))
		}

		switch r.Method {
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(saved)
		default:
			t.Errorf("unexpected method %q", r.Method)
		}
	})

	c := newRegistryClient(t, h)
	toSave := settings.OnboardingSettings{
		RequiredProps:   []string{"docs_url"},
		MCPOptInOffered: boolPtr(true),
	}

	// 1. SetOnboarding
	if err := c.Settings().SetOnboarding(context.Background(), settings.ScopeProject, "prj_foo", toSave); err != nil {
		t.Fatalf("SetOnboarding: %v", err)
	}

	// 2. GetOnboarding
	got, err := c.Settings().GetOnboarding(context.Background(), settings.ScopeProject, "prj_foo")
	if err != nil {
		t.Fatalf("GetOnboarding: %v", err)
	}
	if !reflect.DeepEqual(got, toSave) {
		t.Errorf("got %v, want %v", got, toSave)
	}
}
