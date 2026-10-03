//go:build !unix

package config

// agentCanWrite cannot ask the kernel on this platform, so it assumes the worst:
// an agent could create the directory, and the layer is created and protected.
func agentCanWrite(string) bool { return true }

// agentGetsPast cannot examine ownership here either, so it assumes the worst.
func agentGetsPast(string) (string, error) {
	return "this platform cannot tell who owns the directories above it", nil
}
