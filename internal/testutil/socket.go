// Package testutil provides isolated test fixtures.
package testutil

import (
	"os"
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
