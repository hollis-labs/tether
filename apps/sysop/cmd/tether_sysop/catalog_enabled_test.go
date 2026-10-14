package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyCatalog copies the repository's example catalog so the test can flip one
// agent and one launch to enabled: false without touching the original.
func copyCatalog(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: test fixture inside the repository
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		b = append(b, '\n')
	}
	if err := os.WriteFile(path, append(b, []byte(line+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The catalog response reports each agent's and launch's enabled flag: false only
// where the catalog says enabled: false, true everywhere else (absent = enabled).
func TestHandleCatalogReportsEnabledFlag(t *testing.T) {
	root := t.TempDir()
	copyCatalog(t, filepath.Join("..", "..", "..", "..", "examples", "catalog"), root)
	appendLine(t, filepath.Join(root, "agents", "demo-agent.yaml"), "enabled: false")
	appendLine(t, filepath.Join(root, "launches", "demo-launch.yaml"), "enabled: false")

	s := &appServer{catalogRoot: root}
	rec := httptest.NewRecorder()
	s.handleCatalog(rec, httptest.NewRequest(http.MethodGet, "/api/catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Agents   []agentDTO  `json:"agents"`
		Launches []launchDTO `json:"launches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	agents, launches := map[string]bool{}, map[string]bool{}
	for _, a := range got.Agents {
		agents[a.ID] = a.Enabled
	}
	for _, l := range got.Launches {
		launches[l.ID] = l.Enabled
	}
	if enabled, ok := agents["demo-agent"]; !ok || enabled {
		t.Fatalf("demo-agent enabled = %v (present %v), want false", enabled, ok)
	}
	if enabled, ok := launches["demo-launch"]; !ok || enabled {
		t.Fatalf("demo-launch enabled = %v (present %v), want false", enabled, ok)
	}
	if !agents["worker-agent"] || !launches["task-worker"] {
		t.Fatalf("entries that do not set enabled must report true: agents=%v launches=%v", agents, launches)
	}
	// The key is always present, so the UI never has to guess.
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("response has no explicit enabled:false: %s", rec.Body.String())
	}
}
