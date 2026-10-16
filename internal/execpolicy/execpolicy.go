// Package execpolicy is the daemon-side execution policy: a global go/pause
// switch, a per-environment concurrent-session cap and per-project caps,
// enforced at session launch (CW-20261011-0015). The zero Policy is "running,
// no caps", so nothing changes until an operator sets one.
//
// The environment is the daemon: MaxSessions caps every live session this
// tetherd owns. Admit is a pure decision; the caller supplies the counts.
package execpolicy

import (
	"fmt"
	"sort"
)

// Refusal codes. They are the typed error envelope codes (ADR 0010) a client
// sees on a refused launch.
const (
	CodePaused             = "execution_paused"
	CodeEnvironmentCapFull = "environment_cap_reached"
	CodeProjectCapFull     = "project_cap_reached"
)

// Policy is the operator-set execution policy. A cap of 0 means no cap.
type Policy struct {
	Paused      bool           `json:"paused"`
	Reason      string         `json:"reason,omitempty"`
	MaxSessions int            `json:"max_sessions"`
	ProjectCaps map[string]int `json:"project_caps,omitempty"`
}

// Usage is the live-session count the caps are checked against.
type Usage struct {
	Total     int            `json:"total"`
	ByProject map[string]int `json:"by_project,omitempty"`
}

// Status is the read shape: the policy plus current usage.
type Status struct {
	Policy Policy `json:"policy"`
	Usage  Usage  `json:"usage"`
	// WorkerResume reports catalog.defaults.worker_resume (read-only here; it is
	// configuration, not part of the stored policy).
	WorkerResume bool `json:"worker_resume"`
}

// Validate rejects negative caps and empty project ids.
func (p Policy) Validate() error {
	if p.MaxSessions < 0 {
		return fmt.Errorf("max_sessions must be >= 0")
	}
	for id, n := range p.ProjectCaps {
		if id == "" {
			return fmt.Errorf("project_caps: empty project id")
		}
		if n < 0 {
			return fmt.Errorf("project_caps[%s] must be >= 0", id)
		}
	}
	return nil
}

// Normalize drops zero (no-cap) project entries so equal policies compare equal.
func (p Policy) Normalize() Policy {
	caps := map[string]int{}
	for id, n := range p.ProjectCaps {
		if n > 0 {
			caps[id] = n
		}
	}
	p.ProjectCaps = nil
	if len(caps) > 0 {
		p.ProjectCaps = caps
	}
	if !p.Paused {
		p.Reason = ""
	}
	return p
}

// Refusal is the typed launch refusal. ErrorCode makes the HTTP layer answer
// 409 with Code in the error envelope.
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string     { return r.Message }
func (r *Refusal) ErrorCode() string { return r.Code }

// Admit decides whether one more session for projectID may launch. usage must
// not include the session being admitted.
func Admit(p Policy, u Usage, projectID string) error {
	if p.Paused {
		msg := "execution is paused: no new sessions launch until an operator resumes"
		if p.Reason != "" {
			msg += " (" + p.Reason + ")"
		}
		return &Refusal{Code: CodePaused, Message: msg}
	}
	if p.MaxSessions > 0 && u.Total >= p.MaxSessions {
		return &Refusal{Code: CodeEnvironmentCapFull, Message: fmt.Sprintf("environment session cap reached: %d of %d live sessions", u.Total, p.MaxSessions)}
	}
	if projectCap := p.ProjectCaps[projectID]; projectCap > 0 && u.ByProject[projectID] >= projectCap {
		return &Refusal{Code: CodeProjectCapFull, Message: fmt.Sprintf("project %q session cap reached: %d of %d live sessions", projectID, u.ByProject[projectID], projectCap)}
	}
	return nil
}

// SortedProjects returns the capped project ids in stable order for display.
func (p Policy) SortedProjects() []string {
	ids := make([]string, 0, len(p.ProjectCaps))
	for id := range p.ProjectCaps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
