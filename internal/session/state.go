package session

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
