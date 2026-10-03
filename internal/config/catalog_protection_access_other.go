//go:build !unix

package config

// agentCanWrite cannot ask the kernel on this platform, so it assumes the worst:
// an agent could create the directory, and the layer is created and protected.
func agentCanWrite(string) bool { return true }
