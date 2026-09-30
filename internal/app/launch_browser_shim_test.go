package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/provider/cli/antigravity"
)

func TestWithBrowserShim_AntigravityOnly(t *testing.T) {
	env := []string{"HOME=/h", "PATH=/usr/bin"}

	root := t.TempDir()
	got, err := withBrowserShim("claude-code", root, env)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, env) {
		t.Errorf("non-agy env changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, antigravity.BrowserShimDirName)); !os.IsNotExist(err) {
		t.Errorf("non-agy launch planted a shim: %v", err)
	}

	got, err = withBrowserShim("antigravity", root, env)
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(root, antigravity.BrowserShimDirName)
	want := []string{"HOME=/h", "PATH=" + shim + string(os.PathListSeparator) + "/usr/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("agy env = %q; want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(shim, "open")); err != nil {
		t.Errorf("shim not planted: %v", err)
	}
}
