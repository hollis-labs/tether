package teamruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
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
// catches a stop racing recovery. Replacement changes only the execution ID;
// the original actor/enrollment and receipt key remain authoritative.
func (s *Sessions) Recover(ctx context.Context, key, id string) error {
	unlock := s.lock("session", key)
	defer unlock()
	return s.recoverLocked(ctx, key, id)
}

func (s *Sessions) recoverLocked(ctx context.Context, key, id string) error {
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
	var currentID string
	if readErr := json.Unmarshal(current.payload, &currentID); readErr != nil || currentID == "" {
		return errors.Join(err, teams.ErrConflict, readErr)
	}
	if checkErr := enrollment.ValidateRecovery(ctx, key, request.Actor, currentID); checkErr != nil {
		return errors.Join(err, checkErr, s.service.StopTeamSession(context.WithoutCancel(ctx), currentID))
	}
	return err
}
