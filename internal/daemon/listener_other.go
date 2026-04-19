//go:build !unix

package daemon

// removeStaleSocket is a no-op on non-unix platforms; net.Listen("unix", ...)
// there will return its own error.
func removeStaleSocket(string) error { return nil }
