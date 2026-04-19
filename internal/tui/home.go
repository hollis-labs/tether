package tui

import "os"

// homeDir returns $HOME if set, else the empty string. Kept in its own
// file so tests can monkey-patch it if needed (via build tags).
func homeDir() string {
	return os.Getenv("HOME")
}
