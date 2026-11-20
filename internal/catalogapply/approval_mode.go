package catalogapply

// ApprovalDisabled reports the daemon development-window setting.
func (e *Engine) ApprovalDisabled() bool { return e.ApprovalOff }
