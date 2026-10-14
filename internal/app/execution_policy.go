package app

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/execpolicy"
	"github.com/hollis-labs/tether/internal/identity"
)

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
	return s.SetExecutionPolicyContext(context.Background(), p)
}

// SetExecutionPolicyContext is SetExecutionPolicy plus an audit event naming
// the caller in ctx when the policy actually changed.
func (s *Service) SetExecutionPolicyContext(ctx context.Context, p execpolicy.Policy) (execpolicy.Status, error) {
	if err := p.Validate(); err != nil {
		return execpolicy.Status{}, err
	}
	before, err := s.Store.GetExecutionPolicy()
	if err != nil {
		return execpolicy.Status{}, err
	}
	after := p.Normalize()
	if err := s.Store.SetExecutionPolicy(after); err != nil {
		return execpolicy.Status{}, err
	}
	s.publishPolicyChange(ctx, before.Normalize(), after)
	return s.ExecutionStatus()
}

// publishPolicyChange records who changed the policy and when. It is
// best-effort: the policy is already stored, so a failed publish is dropped.
func (s *Service) publishPolicyChange(ctx context.Context, before, after execpolicy.Policy) {
	if s.Bus == nil || reflect.DeepEqual(before, after) {
		return
	}
	change := "caps_changed"
	switch {
	case !before.Paused && after.Paused:
		change = "paused"
	case before.Paused && !after.Paused:
		change = "resumed"
	}
	actor := ""
	if pr, ok := identity.FromContext(ctx); ok {
		actor = pr.ID
	}
	payload, err := json.Marshal(map[string]any{"change": change, "actor": actor, "at": time.Now().UTC().Format(time.RFC3339), "before": before, "after": after})
	if err != nil {
		return
	}
	_ = s.Bus.Publish(context.WithoutCancel(ctx), events.Event{Scope: events.ScopeDaemon, Kind: events.KindDaemonExecutionPolicyChanged, PayloadJSON: string(payload)})
}

// admitLaunch enforces the execution policy for one session about to launch
// and reserves its slot. Check and reservation happen under one mutex, and
// reservations not yet visible in the store's live count are added to usage,
// so two launches racing at the cap cannot both pass. The caller must call
// the returned release when the launch call returns (by then the session is
// counted by the store or has failed).
func (s *Service) admitLaunch(sessionID, projectID string) (func(), error) {
	p, err := s.Store.GetExecutionPolicy()
	if err != nil {
		return nil, err
	}
	if !p.Paused && p.MaxSessions == 0 && len(p.ProjectCaps) == 0 {
		return func() {}, nil
	}
	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	u, err := s.Store.ExecutionUsage()
	if err != nil {
		return nil, err
	}
	s.addPending(&u, sessionID)
	if err := execpolicy.Admit(p, u, projectID); err != nil {
		return nil, err
	}
	if s.admitPending == nil {
		s.admitPending = map[string]string{}
	}
	s.admitPending[sessionID] = projectID
	return func() {
		s.admitMu.Lock()
		delete(s.admitPending, sessionID)
		s.admitMu.Unlock()
	}, nil
}

// addPending adds reservations not yet visible in the store's live count to u,
// skipping skipID. The caller must hold admitMu.
func (s *Service) addPending(u *execpolicy.Usage, skipID string) {
	for id, proj := range s.admitPending {
		if id == skipID {
			continue
		}
		if row, gerr := s.Store.GetSession(id); gerr == nil && countsAsLive(row.State) {
			continue // already in the store's count
		}
		u.Total++
		if u.ByProject == nil {
			u.ByProject = map[string]int{}
		}
		u.ByProject[proj]++
	}
}

// countsAsLive mirrors Store.ExecutionUsage's state filter.
func countsAsLive(state string) bool {
	switch state {
	case "launching", "running", "detached", "orphaned":
		return true
	}
	return false
}

// Capacity maps the execution status onto CapacityView.
func (s *Service) Capacity() (execpolicy.CapacityView, error) {
	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	st, err := s.ExecutionStatus()
	if err != nil {
		return execpolicy.CapacityView{}, err
	}
	// An in-flight launch holds a slot before the store counts it.
	s.addPending(&st.Usage, "")
	v := execpolicy.CapacityView{EnvironmentID: "local", MaxSessions: st.Policy.MaxSessions, ActiveSessions: st.Usage.Total, Admission: "open", ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	switch {
	case st.Policy.Paused:
		v.Admission, v.Reason = "paused", st.Policy.Reason
	case st.Policy.MaxSessions > 0 && st.Usage.Total >= st.Policy.MaxSessions:
		v.Admission, v.Reason = "full", "environment session cap reached"
	}
	return v, nil
}
