package identity

import "time"

// SetStreamUnavailableGrace replaces streamUnavailableGrace for a test; set it
// before any stream starts and restore it after they have all ended.
func SetStreamUnavailableGrace(d time.Duration) (restore func()) {
	old := streamUnavailableGrace
	streamUnavailableGrace = d
	return func() { streamUnavailableGrace = old }
}
