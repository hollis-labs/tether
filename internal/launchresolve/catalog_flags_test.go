package launchresolve

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The spec engine reads catalog args through ResolveRuntimeBinding. The live
// catalog's opencode.yaml declares `args: [run]`, which agentkit v0.12.0's
// providerplant refuses after the projected argv; the binding drops it as
// the catalog engine does (config.CatalogFlags), and keeps the rest.
func TestResolveRuntimeBinding_OpencodeCatalogRunArg(t *testing.T) {
	root := t.TempDir()
	src := fixtureRoot(t)
	if err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o750)
		}
		data, err := os.ReadFile(path) //nolint:gosec // test fixture path
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	}); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	opencode := "id: opencode\ntype: cli\nargs:\n  - run\n  - --print-logs\nbootstrap:\n  mode: prepend\n"
	if err := os.WriteFile(filepath.Join(root, "providers", "opencode.yaml"), []byte(opencode), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := OpenAt(Options{CatalogRoot: root})
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	got, err := reg.ResolveRuntimeBinding("opencode")
	if err != nil {
		t.Fatalf("ResolveRuntimeBinding: %v", err)
	}
	if !slices.Equal(got.Args, []string{"--print-logs"}) {
		t.Fatalf("binding Args = %q, want [--print-logs] (leading run dropped)", got.Args)
	}
}
