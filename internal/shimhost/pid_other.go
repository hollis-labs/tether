//go:build !linux && !windows

package shimhost

// Other hosts retain the pid in the placement receipt. Unknown pids remain
// unknown rather than being inferred from provider health.
func peerPID(_ string) (int, error) { return 0, nil }
