package setup_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/setup"
)

// TestDetectProviders verifies that DetectProviders returns one row per known
// brand, and that a brand whose binary is on the test PATH is reported Found.
func TestDetectProviders(t *testing.T) {
	// Seed a temporary directory with a stub "claude" binary so detection
	// returns a concrete path.
	tmpDir := t.TempDir()
	stub := filepath.Join(tmpDir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Clear any existing CLAUDE_CLI_PATH so PATH lookup is exercised.
	t.Setenv("CLAUDE_CLI_PATH", "")

	results := setup.DetectProviders()
	if got, want := len(results), 3; got != want {
		t.Fatalf("len(results) = %d, want %d", got, want)
	}

	brands := make(map[string]setup.DetectResult, len(results))
	for _, r := range results {
		brands[r.Brand] = r
	}

	for _, brand := range []string{"claude", "codex", "opencode"} {
		r, ok := brands[brand]
		if !ok {
			t.Errorf("missing brand %q", brand)
			continue
		}
		if brand == "claude" {
			if !r.Found {
				t.Errorf("claude: Found = false, want true (stub is on PATH)")
			}
			if r.Path != stub {
				t.Errorf("claude: Path = %q, want %q", r.Path, stub)
			}
			if r.Source != "PATH" {
				t.Errorf("claude: Source = %q, want %q", r.Source, "PATH")
			}
		} else {
			// codex and opencode are not on our stub PATH — expect not found
			// unless they're coincidentally installed.
			if r.Brand != brand {
				t.Errorf("unexpected brand %q", r.Brand)
			}
		}
	}
}

// TestDetectProvidersEnvVar verifies that $CLAUDE_CLI_PATH is honored and
// the Source field is annotated accordingly.
func TestDetectProvidersEnvVar(t *testing.T) {
	tmpDir := t.TempDir()
	stub := filepath.Join(tmpDir, "my-claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("CLAUDE_CLI_PATH", stub)

	results := setup.DetectProviders()
	for _, r := range results {
		if r.Brand == "claude" {
			if !r.Found {
				t.Fatal("claude: Found = false with CLAUDE_CLI_PATH set")
			}
			if r.Source != "env:CLAUDE_CLI_PATH" {
				t.Errorf("claude: Source = %q, want %q", r.Source, "env:CLAUDE_CLI_PATH")
			}
			return
		}
	}
	t.Fatal("claude brand not found in results")
}

// TestWriteCatalogBootable verifies that a WriteCatalog result can be loaded
// by config.LoadLayered without error.
func TestWriteCatalogBootable(t *testing.T) {
	dst := t.TempDir()
	rep, err := setup.WriteCatalog(dst, setup.WriteOpts{})
	if err != nil {
		t.Fatalf("WriteCatalog: %v", err)
	}
	if len(rep.Written) == 0 {
		t.Fatal("expected at least one written file")
	}

	cat, err := config.LoadLayered(filepath.Join(dst, "catalog"))
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestWriteCatalogMinimal verifies the minimal seed produces a bootable catalog.
func TestWriteCatalogMinimal(t *testing.T) {
	dst := t.TempDir()
	rep, err := setup.WriteCatalog(dst, setup.WriteOpts{Minimal: true})
	if err != nil {
		t.Fatalf("WriteCatalog minimal: %v", err)
	}

	// Minimal should write global.yaml + one provider.
	wantFiles := []string{
		filepath.Join("catalog", "global.yaml"),
		filepath.Join("catalog", "providers", "claude-code.yaml"),
	}
	written := make(map[string]bool, len(rep.Written))
	for _, f := range rep.Written {
		written[f] = true
	}
	for _, f := range wantFiles {
		if !written[f] {
			t.Errorf("minimal: missing expected file %q", f)
		}
	}

	cat, err := config.LoadLayered(filepath.Join(dst, "catalog"))
	if err != nil {
		t.Fatalf("LoadLayered minimal: %v", err)
	}
	if err := cat.Validate(); err != nil {
		t.Fatalf("Validate minimal: %v", err)
	}
}

// TestWriteCatalogIdempotent verifies that a second run skips already-written
// files, and Force:true backs them up.
func TestWriteCatalogIdempotent(t *testing.T) {
	dst := t.TempDir()

	// First write.
	rep1, err := setup.WriteCatalog(dst, setup.WriteOpts{})
	if err != nil {
		t.Fatalf("first WriteCatalog: %v", err)
	}
	firstCount := len(rep1.Written)
	if firstCount == 0 {
		t.Fatal("first run: nothing written")
	}

	// Second write without Force — all files should be skipped.
	rep2, err := setup.WriteCatalog(dst, setup.WriteOpts{})
	if err != nil {
		t.Fatalf("second WriteCatalog: %v", err)
	}
	if len(rep2.Written) != 0 {
		t.Errorf("second run wrote %d files, want 0", len(rep2.Written))
	}
	if len(rep2.Skipped) != firstCount {
		t.Errorf("second run skipped %d files, want %d", len(rep2.Skipped), firstCount)
	}

	// Third write with Force — all files overwritten + backed up.
	rep3, err := setup.WriteCatalog(dst, setup.WriteOpts{Force: true})
	if err != nil {
		t.Fatalf("force WriteCatalog: %v", err)
	}
	if len(rep3.Written) != firstCount {
		t.Errorf("force run wrote %d files, want %d", len(rep3.Written), firstCount)
	}
	if len(rep3.Backed) != firstCount {
		t.Errorf("force run backed up %d files, want %d", len(rep3.Backed), firstCount)
	}
	// Verify backups exist.
	for _, bak := range rep3.Backed {
		if _, err := os.Stat(bak); err != nil {
			t.Errorf("backup %s does not exist: %v", bak, err)
		}
	}
}

// TestWriteCatalogProviderCommands verifies that ProviderCommands stamps the
// resolved path into the correct provider YAML.
func TestWriteCatalogProviderCommands(t *testing.T) {
	dst := t.TempDir()
	fakePath := "/opt/homebrew/bin/claude"

	_, err := setup.WriteCatalog(dst, setup.WriteOpts{
		ProviderCommands: map[string]string{"claude": fakePath},
	})
	if err != nil {
		t.Fatalf("WriteCatalog: %v", err)
	}

	providerFile := filepath.Join(dst, "catalog", "providers", "claude-code.yaml")
	data, err := os.ReadFile(providerFile) //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("read provider file: %v", err)
	}

	if !containsLine(string(data), "command: "+fakePath) {
		t.Errorf("stamped provider file does not contain %q:\n%s", "command: "+fakePath, data)
	}
}

// TestMCPServerExample verifies the embedded MCP server example parses.
func TestMCPServerExample(t *testing.T) {
	dst := t.TempDir()
	if _, err := setup.WriteCatalog(dst, setup.WriteOpts{}); err != nil {
		t.Fatalf("WriteCatalog: %v", err)
	}

	servers, err := config.LoadMCPServerCatalog(filepath.Join(dst, "catalog"))
	if err != nil {
		t.Fatalf("LoadMCPServerCatalog: %v", err)
	}
	if len(servers) == 0 {
		t.Fatal("expected at least one MCP server entry in seed catalog")
	}
	found := false
	for _, s := range servers {
		if s.ID == "mcp-filesystem" {
			found = true
		}
	}
	if !found {
		t.Errorf("mcp-filesystem entry not found; got: %v", servers)
	}
}

func containsLine(s, needle string) bool {
	for _, line := range splitLines(s) {
		if line == needle {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
