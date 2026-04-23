package session

import "errors"

type State string

const (
	StateCreated   State = "created"
	StateReady     State = "ready"
	StateLaunching State = "launching"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateKilled    State = "killed"
)

// ErrNotCreated is returned when a LaunchSession call is attempted on a
// session that is not in the "created" state. Use errors.Is to check.
// Defined here (not in app) to allow the api and app packages to both
// reference it without a cycle.
var ErrNotCreated = errors.New("session is not in 'created' state")
