//go:build !unix

package credfile

import "os"

func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: operator-configured credential path, validated by Read
}

// Ownership is not checked off unix; Tether targets linux and darwin.
func ownedByCurrentUID(os.FileInfo) bool { return true }
