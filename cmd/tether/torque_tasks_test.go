package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestBuildTorqueTaskLaunchAugment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tasks/CW-1" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":            "CW-1",
			"title":         "Fix small thing",
			"description":   "Do the narrow fix.",
			"status":        "doing",
			"priority":      2,
			"working_dir":   "/tmp/work",
			"agent_profile": "implementer",
			"subtodos": []map[string]any{
				{"title": "Patch code", "done": false},
			},
		})
	}))
	defer srv.Close()

	inj, prompt, err := buildTorqueTaskLaunchAugment(context.Background(), srv.URL, []string{"CW-1"}, "tasks")
	if err != nil {
		t.Fatalf("buildTorqueTaskLaunchAugment: %v", err)
	}
	if len(inj.NativeFiles) != 4 {
		t.Fatalf("NativeFiles len = %d, want 4", len(inj.NativeFiles))
	}
	paths := map[string]string{}
	for _, f := range inj.NativeFiles {
		paths[f.RelPath] = f.Content
	}
	for _, rel := range []string{"tasks/README.md", "tasks/CW-1/task.json", "tasks/CW-1/task.md", "tasks/CW-1/process.md"} {
		if paths[rel] == "" {
			t.Fatalf("missing planted file %s; paths=%v", rel, paths)
		}
	}
	if !strings.Contains(paths["tasks/CW-1/process.md"], srv.URL+"/api/v1/tasks/CW-1/transition") {
		t.Fatalf("process commands missing exact task URL:\n%s", paths["tasks/CW-1/process.md"])
	}
	if !strings.Contains(prompt, "tasks/README.md") || !strings.Contains(prompt, "$CODEX_HOME/tasks/README.md") || !strings.Contains(prompt, "CW-1") {
		t.Fatalf("prompt append missing task reference: %q", prompt)
	}
}

func TestBuildTorqueTaskLaunchAugmentRejectsUnsafeTaskDir(t *testing.T) {
	_, _, err := buildTorqueTaskLaunchAugment(context.Background(), "http://127.0.0.1:8990", []string{"CW-1"}, "../tasks")
	if err == nil {
		t.Fatal("expected unsafe task dir error")
	}
	if !strings.Contains(err.Error(), "safe bootdir-relative path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildTorqueTaskLaunchAugmentRejectsDuplicateTaskIDs(t *testing.T) {
	_, _, err := buildTorqueTaskLaunchAugment(context.Background(), "http://127.0.0.1:8990", []string{"CW-1", "CW-1"}, "tasks")
	if err == nil {
		t.Fatal("expected duplicate task id error")
	}
	if !strings.Contains(err.Error(), "duplicate Torque task id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildTorqueTaskLaunchAugmentRejectsCollidingTaskDirs(t *testing.T) {
	_, _, err := buildTorqueTaskLaunchAugment(context.Background(), "http://127.0.0.1:8990", []string{"CW 1", "CW/1"}, "tasks")
	if err == nil {
		t.Fatal("expected colliding task dir error")
	}
	if !strings.Contains(err.Error(), "both map to bundle directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMergeLaunchInjectionJSON(t *testing.T) {
	existing := `{"native_files":[{"kind":"raw","rel_path":"notes/base.md","content":"base"}]}`
	extra := config.LaunchInjection{NativeFiles: []config.InjectedFile{{
		Kind:    "raw",
		RelPath: "tasks/README.md",
		Content: "task bundle",
	}}}
	merged, err := mergeLaunchInjectionJSON(existing, extra)
	if err != nil {
		t.Fatalf("mergeLaunchInjectionJSON: %v", err)
	}
	if !strings.Contains(merged, "notes/base.md") || !strings.Contains(merged, "tasks/README.md") {
		t.Fatalf("merged injection missing expected files: %s", merged)
	}
}
