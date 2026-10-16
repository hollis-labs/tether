package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestWorkerSpecCarriesExtractRefs(t *testing.T) {
	s := &Service{Catalog: &config.Catalog{}}
	spec, err := s.workerSpec(context.Background(), "worker-session", &launch.Plan{ProviderBrand: "claude", ExtractRefs: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ExtractRefs bool `json:"extract_refs"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.ExtractRefs {
		t.Fatal("worker launch loses resolved extract_refs policy")
	}
}

// A worker cannot use a path on the hub's machine, so the spec must carry something it can clone.
func TestWorkerSpecWorkspaceFromRepoURL(t *testing.T) {
	s := &Service{Catalog: &config.Catalog{}}
	cases := []struct {
		name     string
		plan     launch.Plan
		wantKind string
		wantURL  string
	}{
		{"repo_url wins over a hub path", launch.Plan{ProviderBrand: "claude", RepoRoot: "/home/hub/dev/tether", RepoURL: "https://github.com/hollis-labs/tether.git"}, "git", "https://github.com/hollis-labs/tether.git"},
		{"repo_url wins over a url-shaped repo_root", launch.Plan{ProviderBrand: "claude", RepoRoot: "https://github.com/other/x.git", RepoURL: "https://github.com/hollis-labs/tether.git"}, "git", "https://github.com/hollis-labs/tether.git"},
		{"url-shaped repo_root still works", launch.Plan{ProviderBrand: "claude", RepoRoot: "https://github.com/hollis-labs/torque.git"}, "git", "https://github.com/hollis-labs/torque.git"},
		{"no remote and no local checkout falls back to scratch", launch.Plan{ProviderBrand: "claude", RepoRoot: "/does/not/exist/on/this/host"}, "scratch", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := tc.plan
			spec, err := s.workerSpec(context.Background(), "sess-1", &plan)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Workspace.Kind != tc.wantKind || spec.Workspace.URL != tc.wantURL {
				t.Fatalf("workspace = %+v, want kind=%q url=%q", spec.Workspace, tc.wantKind, tc.wantURL)
			}
		})
	}
}

func TestWorkerSpecCarriesPermissionMode(t *testing.T) {
	s := &Service{Catalog: &config.Catalog{}}
	for _, mode := range []string{"bypass", "default", ""} {
		plan := launch.Plan{ProviderBrand: "claude", PermissionMode: mode}
		spec, err := s.workerSpec(context.Background(), "sess-1", &plan)
		if err != nil {
			t.Fatal(err)
		}
		if spec.PermissionMode != mode {
			t.Fatalf("spec.PermissionMode = %q, want %q", spec.PermissionMode, mode)
		}
	}
}

func TestWorkerSpecCarriesResumeSessionID(t *testing.T) {
	s := &Service{Catalog: &config.Catalog{}}
	spec, err := s.workerSpec(context.Background(), "sess-1", &launch.Plan{ProviderBrand: "claude", ResumeProviderSessionID: "native-1"})
	if err != nil || spec.ResumeSessionID != "native-1" {
		t.Fatalf("spec resume id = %q err=%v", spec.ResumeSessionID, err)
	}
	spec, _ = s.workerSpec(context.Background(), "sess-2", &launch.Plan{ProviderBrand: "claude"})
	if spec.ResumeSessionID != "" {
		t.Fatalf("fresh launch carries resume id %q", spec.ResumeSessionID)
	}
}
