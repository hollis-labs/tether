package teamruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/teamhost"
)

type retainedSessionService interface {
	RecoverTeamSession(context.Context, string, string) error
}

type retainedEnrollment interface {
	ValidateRecovery(context.Context, string, mesh.URN, string) error
}

// Recover serializes with Launch/Stop using the original receipt. Stop records
// its tombstone before taking this lock, so app's transactional fence also
// catches a stop racing recovery. Neither path substitutes a session/actor.
func (s *Sessions) Recover(ctx context.Context, key, id string) error {
	unlock := s.lock("session", key)
	defer unlock()
	receipt, err := s.read(ctx, "session", key)
	if err != nil {
		return err
	}
	if receipt.ended != "" || receipt.state != "done" {
		return teams.ErrDenied
	}
	var savedID string
	var request teamhost.SessionRequest
	if err = json.Unmarshal(receipt.payload, &savedID); err != nil {
		return err
	}
	if err = json.Unmarshal(receipt.request, &request); err != nil {
		return err
	}
	if savedID != id || request.IntentKey != key || request.Actor == "" {
		return teams.ErrConflict
	}
	if _, err = s.enrollment.LaunchTarget(ctx, key, request.Actor); err != nil {
		return err
	}
	enrollment, ok := s.enrollment.(retainedEnrollment)
	if !ok {
		return teamhost.ErrSessionUnavailable
	}
	if err = enrollment.ValidateRecovery(ctx, key, request.Actor, id); err != nil {
		return err
	}
	service, ok := s.service.(retainedSessionService)
	if !ok {
		return teamhost.ErrSessionUnavailable
	}
	err = service.RecoverTeamSession(ctx, key, id)
	current, readErr := s.read(ctx, "session", key)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if current.ended != "" {
		return errors.Join(teams.ErrDenied, s.cleanup(context.WithoutCancel(ctx), key))
	}
	if checkErr := enrollment.ValidateRecovery(ctx, key, request.Actor, id); checkErr != nil {
		return errors.Join(err, checkErr, s.service.StopTeamSession(context.WithoutCancel(ctx), id))
	}
	return err
}

func (e *LegacyEnroller) ValidateRecovery(ctx context.Context, key string, actor mesh.URN, id string) error {
	r, err := e.read(ctx, "enrollment", key)
	if err != nil {
		return err
	}
	if r.ended != "" || r.bindingEnded || r.state != "done" {
		return teams.ErrDenied
	}
	var saved enrollmentReceipt
	if err = json.Unmarshal(r.payload, &saved); err != nil {
		return err
	}
	if saved.Enrollment.Actor != actor || saved.Enrollment.AgentID != string(actor) {
		return teams.ErrConflict
	}
	profile, err := e.registry.Lookup(ctx, string(actor))
	if err != nil {
		return err
	}
	var pin mesh.DefinitionRef
	if err = json.Unmarshal([]byte(profile.Props["team_definition"]), &pin); err != nil {
		return err
	}
	if profile.Status != registry.StatusActive || profile.Kind != registry.KindAgent || pin != saved.Pin {
		return teams.ErrDenied
	}
	binding, err := e.registry.CurrentBinding(ctx, string(actor))
	if err != nil {
		return err
	}
	if binding.SessionID != id || binding.AttemptID != r.bindingSecret || binding.HostID != "team" || binding.Visibility != registry.VisibilityTetherHosted {
		return teams.ErrUnavailable
	}
	return nil
}
