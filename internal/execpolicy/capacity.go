package execpolicy

// CapacityView is the flat read shape Torque polls (CW-20261011-0015).
type CapacityView struct {
	EnvironmentID  string `json:"environment_id"`
	MaxSessions    int    `json:"max_sessions"`
	ActiveSessions int    `json:"active_sessions"`
	Admission      string `json:"admission"` // open | paused | full
	Reason         string `json:"reason,omitempty"`
	ObservedAt     string `json:"observed_at"`
}
