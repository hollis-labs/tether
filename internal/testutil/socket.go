// Package testutil provides isolated test fixtures.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// SocketDir avoids t.TempDir's test-name hierarchy in Unix socket paths.
// It keeps the socket fixture under the configured temporary root.
func SocketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ShortDir prefers the configured temporary root while leaving room for the
// session hash and socket suffix. An overlong root falls back to the OS's short
// temporary directory; every fixture registers removal before returning.
func ShortDir(t testing.TB) string {
	t.Helper()
	base := os.TempDir()
	if len(base) > 40 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

// CanonicalizeTempEnv points TMPDIR at the symlink-resolved temporary root so
// every later t.TempDir is canonical. On macOS os.TempDir is under /var, a
// symlink to /private/var, and production path checks refuse non-canonical
// roots. Call it from TestMain before m.Run.
func CanonicalizeTempEnv() error {
	resolved, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return err
	}
	return os.Setenv("TMPDIR", resolved)
}
