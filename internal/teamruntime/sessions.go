package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

// SessionService is satisfied by app.Service. Every runtime mutation stays at
// that daemon boundary; tests can supply a harmless runtime implementation.
type SessionService interface {
	CreateTeamSession(context.Context, string, string) (*app.Launched, error)
	LaunchSessionWithContext(context.Context, string) (*app.Launched, error)
	LinkTeamSession(context.Context, string, string, string) error
	StopTeamSession(context.Context, string) error
}

// SessionEnrollment is the enrollment adapter seam needed before Start.
type SessionEnrollment interface {
	LaunchTarget(context.Context, string, mesh.URN) (string, error)
	BindSession(context.Context, string, mesh.URN, string) error
}

type Sessions struct {
	receipts
	service    SessionService
	sessions   *store.Store
	enrollment SessionEnrollment
}

func NewSessions(db *store.Store, s *teamstore.Store, service SessionService, enrollment SessionEnrollment) (*Sessions, error) {
	if db == nil || s == nil || service == nil || enrollment == nil {
		return nil, errors.New("team sessions: store, session service and enrollment adapter required")
	}
	return &Sessions{receipts: receipts{db.DB(), s}, service: service, sessions: db, enrollment: enrollment}, nil
}
func sessionKey(key string) string { return "team-port:" + key }
func (s *Sessions) Launch(ctx context.Context, in teamhost.SessionRequest) (string, error) {
	unlock := s.lock("session", in.IntentKey)
	defer unlock()
	saved, err := s.reserve(ctx, "session", in.IntentKey, in)
	if err != nil {
		return "", err
	}
	if saved.state == "done" {
		var id string
		if err := json.Unmarshal(saved.payload, &id); err != nil {
			return "", err
		}
		if _, err := s.sessions.SessionReplacementDestination(ctx, id); err == nil {
			return id, s.recoverLocked(ctx, in.IntentKey, id)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	launchID, err := s.enrollment.LaunchTarget(ctx, in.IntentKey, in.Actor)
	if err != nil {
		return "", err
	}
	launched, err := s.service.CreateTeamSession(ctx, sessionKey(saved.nonce), launchID)
	if err != nil {
		return "", classifySession(err)
	}
	if err = s.service.LinkTeamSession(ctx, launched.SessionID, sessionKey(saved.nonce), in.ParentSession); err != nil {
		return "", classifySession(err)
	}
	// Publishing the session in the receipt precedes Start. Stop may already have
	// persisted a tombstone; its keyed lookup also finds a lost create ack.
	if err = s.complete(ctx, "session", in.IntentKey, launched.SessionID); err != nil {
		return "", errors.Join(err, s.cleanup(context.WithoutCancel(ctx), in.IntentKey))
	}
	if err = s.enrollment.BindSession(ctx, in.IntentKey, in.Actor, launched.SessionID); err != nil {
		return "", err
	}
	_, err = s.service.LaunchSessionWithContext(ctx, launched.SessionID)
	if err != nil {
		return "", classifySession(err)
	}
	current, err := s.read(ctx, "session", in.IntentKey)
	if err != nil {
		return "", err
	}
	if current.ended != "" {
		return "", errors.Join(teams.ErrDenied, s.cleanup(context.WithoutCancel(ctx), in.IntentKey))
	}
	row, err := s.sessions.GetSession(launched.SessionID)
	if err != nil {
		return "", err
	}
	if session.State(row.State).Terminal() {
		return "", fmt.Errorf("%w: %w", teams.ErrProvisionFailed, teamhost.ErrSessionGone)
	}
	return launched.SessionID, nil
}
func (s *Sessions) Stop(ctx context.Context, key string) error {
	if err := s.end(ctx, "session", key, "stop", false); err != nil {
		return err
	}
	unlock := s.lock("session", key)
	defer unlock()
	return s.cleanup(ctx, key)
}
func (s *Sessions) cleanup(ctx context.Context, key string) error {
	receipt, err := s.read(ctx, "session", key)
	if err != nil {
		return err
	}
	if receipt.ended == "" {
		return nil
	}
	// A stop-before-acquisition tombstone has no nonce and owns no session.
	if receipt.nonce == "" {
		return s.cleaned(ctx, "session", key)
	}
	record, err := s.sessions.GetSessionIdempotency(sessionKey(receipt.nonce))
	if err != nil {
		return err
	}
	if record == nil {
		return s.cleaned(ctx, "session", key)
	}
	if err = s.service.StopTeamSession(ctx, record.SessionID); err != nil {
		return err
	}
	return s.cleaned(ctx, "session", key)
}

func classifySession(err error) error {
	if errors.Is(err, store.ErrIdempotencyConflict) || errors.Is(err, store.ErrSessionNotFound) || errors.Is(err, session.ErrNotCreated) {
		return fmt.Errorf("%w: %w: %w", teams.ErrProvisionFailed, teamhost.ErrPermanent, err)
	}
	return err
}
