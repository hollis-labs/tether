package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// isolateTetherPaths pins HOME and the four $XDG_*_HOME roots into the test's
// temp dir so `mux path` resolves Tether's go-apppaths layout entirely under
// t.TempDir, never the developer's real home. Per the cutover runbook's sharp
// edge #5, pin the XDG vars (and HOME) into the temp dir.
func isolateTetherPaths(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "xdg-cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
}

// writeCatalog writes a minimal catalog rooted at dir with the given
// global.yaml body and returns the catalog root.
func writeCatalog(t *testing.T, dir, globalBody string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte(globalBody), 0o600); err != nil {
		t.Fatalf("write global.yaml: %v", err)
	}
	return dir
}

// TestPathCmd_FallbackWhenCatalogOmitsKeys runs `mux path` against a catalog
// whose global.yaml omits state_db / workspace_root / temp_root and confirms
// the printed effective paths are the go-apppaths fallbacks under the
// hermetic XDG roots.
func TestPathCmd_FallbackWhenCatalogOmitsKeys(t *testing.T) {
	dir := t.TempDir()
	isolateTetherPaths(t, dir)
	catalogPath = writeCatalog(t, t.TempDir(), "version: 0.1.0\n")

	var out bytes.Buffer
	cmd := pathCmd()
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("path cmd: %v", err)
	}
	got := out.String()

	wantDB := filepath.Join(dir, "xdg-data", "tether", "workspaces", "default", "main.db")
	wantWS := filepath.Join(dir, "xdg-data", "tether", "workspaces")
	wantTmp := filepath.Join(dir, "xdg-cache", "tether")
	for _, want := range []string{wantDB, wantWS, wantTmp, "go-apppaths fallback"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("path output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestPathCmd_ExplicitCatalogValuesWin runs `mux path` against a catalog whose
// global.yaml sets state_db / workspace_root / temp_root explicitly and
// confirms the printed effective paths are the catalog values (labeled
// "catalog"), not the go-apppaths fallback — the precedence guard.
func TestPathCmd_ExplicitCatalogValuesWin(t *testing.T) {
	dir := t.TempDir()
	isolateTetherPaths(t, dir)

	stateDB := filepath.Join(dir, "explicit", "tether.db")
	wsRoot := filepath.Join(dir, "explicit", "workspaces")
	tmpRoot := filepath.Join(dir, "explicit", "tmp")
	body := "version: 0.1.0\n" +
		"catalog:\n" +
		"  defaults:\n" +
		"    state_db: " + stateDB + "\n" +
		"    workspace_root: " + wsRoot + "\n" +
		"    temp_root: " + tmpRoot + "\n"
	catalogPath = writeCatalog(t, t.TempDir(), body)

	var out bytes.Buffer
	cmd := pathCmd()
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("path cmd: %v", err)
	}
	got := out.String()

	for _, want := range []string{stateDB, wsRoot, tmpRoot, "(catalog)"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("path output missing %q\n--- output ---\n%s", want, got)
		}
	}
	// The go-apppaths fallback main.db must NOT appear as an effective value.
	fallbackDB := filepath.Join(dir, "xdg-data", "tether", "workspaces", "default", "main.db")
	if bytes.Contains([]byte(got), []byte(fallbackDB+"\t(go-apppaths fallback)")) {
		t.Errorf("path output used the go-apppaths fallback %q despite an explicit catalog state_db", fallbackDB)
	}
}
