package app

import "github.com/hollis-labs/tether/internal/execpolicy"

// ExecutionStatus returns the stored execution policy and current live usage.
func (s *Service) ExecutionStatus() (execpolicy.Status, error) {
	p, err := s.Store.GetExecutionPolicy()
	if err != nil {
		return execpolicy.Status{}, err
	}
	u, err := s.Store.ExecutionUsage()
	if err != nil {
		return execpolicy.Status{}, err
	}
	return execpolicy.Status{Policy: p, Usage: u}, nil
}

// SetExecutionPolicy validates and stores the policy. It does not touch
// running sessions: caps and pause only gate new launches.
func (s *Service) SetExecutionPolicy(p execpolicy.Policy) (execpolicy.Status, error) {
	if err := p.Validate(); err != nil {
		return execpolicy.Status{}, err
	}
	if err := s.Store.SetExecutionPolicy(p.Normalize()); err != nil {
		return execpolicy.Status{}, err
	}
	return s.ExecutionStatus()
}

// admitLaunch enforces the execution policy for one session about to launch.
// The check and the state change are not one transaction, so two launches
// racing at the cap can both pass; the cap is a launch gate, not a hard limit.
func (s *Service) admitLaunch(projectID string) error {
	p, err := s.Store.GetExecutionPolicy()
	if err != nil {
		return err
	}
	if !p.Paused && p.MaxSessions == 0 && len(p.ProjectCaps) == 0 {
		return nil
	}
	u, err := s.Store.ExecutionUsage()
	if err != nil {
		return err
	}
	return execpolicy.Admit(p, u, projectID)
}
