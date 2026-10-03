package session

import "errors"

// State is Tether's durable lifecycle vocabulary. Mesh SessionState maps
// created/ready/launching to starting, running/detached/orphaned directly,
// and completed/failed/killed to ended; Tether has no paused state yet.
// Detached here describes the daemon connection, not client detach
// (client_attachments.detached_at / CLI Ctrl-]), which is unchanged.
type State string

const (
	StateCreated   State = "created"
	StateReady     State = "ready"
	StateLaunching State = "launching"
	StateRunning   State = "running"
	StateDetached  State = "detached"
	StateOrphaned  State = "orphaned"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateKilled    State = "killed"
)

// ErrNotCreated is returned when a LaunchSession call is attempted on a
// session that is not in the "created" state. Use errors.Is to check.
// Defined here (not in app) to allow the api and app packages to both
// reference it without a cycle.
var ErrNotCreated = errors.New("session is not in 'created' state")

// Terminal reports an observed end or a failed launch/recovery.
// Detached and orphaned are non-terminal and must retain their workspaces.
func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateKilled
}

// ErrDetached is returned when resume would duplicate a still-live child.
var ErrDetached = errors.New("session is detached and still alive; wait for the shim reconciler to reattach it before resuming")
