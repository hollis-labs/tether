package config

import (
	"path/filepath"
	"testing"
)

func TestLoadA2ABindings(t *testing.T) {
	t.Run("valid entry", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "a2a", "greeter.yaml"), `
id: greeter
target_urn: "msg://agent/agent-mux/agt_greeter00000"
display_name: "Greeter"
description: "says hello"
base_url: "https://example.com/a2a/agents/greeter"
task_mode: false
`)
		entries, err := LoadA2ABindings(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		e := entries[0]
		if e.ID != "greeter" || e.TargetURN != "msg://agent/agent-mux/agt_greeter00000" {
			t.Errorf("entry = %+v", e)
		}
		if e.TaskMode {
			t.Errorf("TaskMode = true, want false")
		}
	})

	t.Run("env var expansion in base_url and bearer_token", func(t *testing.T) {
		t.Setenv("TEST_A2A_BASE_URL", "https://host.example/a2a/agents/x")
		t.Setenv("TEST_A2A_TOKEN", "secret-tok")
		dir := t.TempDir()
		write(t, filepath.Join(dir, "a2a", "x.yaml"), `
id: x
target_urn: "msg://agent/agent-mux/agt_x0000000000"
base_url: "${TEST_A2A_BASE_URL}"
bearer_token: "${TEST_A2A_TOKEN}"
`)
		entries, err := LoadA2ABindings(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].BaseURL != "https://host.example/a2a/agents/x" {
			t.Errorf("BaseURL = %q", entries[0].BaseURL)
		}
		if entries[0].BearerToken != "secret-tok" {
			t.Errorf("BearerToken = %q", entries[0].BearerToken)
		}
	})

	t.Run("disabled entry excluded from LoadA2ABindings but visible in catalog", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "a2a", "off.yaml"), `
id: off
target_urn: "msg://agent/agent-mux/agt_off00000000"
base_url: "https://x/agents/off"
enabled: false
`)
		entries, err := LoadA2ABindings(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected 0 enabled entries, got %d", len(entries))
		}

		catalog, err := LoadA2ABindingCatalog(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(catalog) != 1 || catalog[0].IsEnabled() {
			t.Fatalf("catalog = %+v, want 1 disabled entry visible", catalog)
		}
	})

	t.Run("missing a2a dir is empty, not an error", func(t *testing.T) {
		dir := t.TempDir() // no a2a subdir
		entries, err := LoadA2ABindings(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("expected 0 entries, got %d", len(entries))
		}
	})

	t.Run("malformed yaml surfaces an error", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "a2a", "bad.yaml"), `{invalid yaml: [`)
		if _, err := LoadA2ABindings(dir); err == nil {
			t.Fatal("expected an error for malformed yaml")
		}
	})

	t.Run("TaskAwaitTimeout converts seconds, zero means unset", func(t *testing.T) {
		e := A2ABindingEntry{TaskAwaitTimeoutSeconds: 45}
		if got := e.TaskAwaitTimeout(); got.Seconds() != 45 {
			t.Errorf("TaskAwaitTimeout() = %v, want 45s", got)
		}
		zero := A2ABindingEntry{}
		if got := zero.TaskAwaitTimeout(); got != 0 {
			t.Errorf("TaskAwaitTimeout() with unset seconds = %v, want 0", got)
		}
	})
}
