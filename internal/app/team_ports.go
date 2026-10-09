package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/mesh"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

// LinkTeamSession sets a retained team parent before the first launch. It is an
// internal adapter operation: the key must already own the created session.
func (s *Service) LinkTeamSession(ctx context.Context, id, key, parent string) error {
	result, err := s.Store.DB().ExecContext(ctx, `UPDATE sessions SET parent_session_id=NULLIF(?,'') WHERE id=? AND state='created' AND EXISTS(SELECT 1 FROM session_idempotency WHERE key=? AND session_id=?)`, parent, id, key, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	row, err := s.Store.GetSession(id)
	if err != nil {
		return err
	}
	record, err := s.Store.GetSessionIdempotency(key)
	if err != nil {
		return err
	}
	if record == nil || record.SessionID != id || row.ParentSessionID.String != parent {
		return teams.ErrConflict
	}
	return nil
}

// QueueTeamDelivery accepts a keyed retained-session turn in the existing
// durable idle queue. Empty policy also allows queued delivery; unsupported
// policies are refused. No actor successor is supplied, so the dispatcher may
// never retarget this turn after the retained session ends.
func (s *Service) QueueTeamDelivery(ctx context.Context, d teams.Delivery) error {
	if d.Delivery != "" && d.Delivery != mesh.DeliveryAtIdle {
		return teamhost.ErrInvalidRequest
	}
	if d.Recipient.SessionID == "" && d.Recipient.Kind != mesh.ActorAgent {
		from, err := messaging.ParseURN(string(d.From))
		if err != nil {
			return teamhost.ErrInvalidRequest
		}
		to, err := messaging.ParseURN(string(d.Recipient.Actor))
		if err != nil {
			return teamhost.ErrInvalidRequest
		}
		body, err := json.Marshal(struct {
			Body string `json:"body"`
		}{d.Body})
		if err != nil {
			return err
		}
		request, err := json.Marshal(d)
		if err != nil {
			return err
		}
		_, err = s.Store.SendKeyedMessage(ctx, d.IdempotencyKey, messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: body, ContentType: "application/json", Metadata: map[string]string{"team_run": d.Route.RunID, "team_delivery_key": d.IdempotencyKey, "team_delivery_request": string(request)}})
		return err
	}
	dispatcher := s.replies.Load()
	if dispatcher == nil {
		return teamhost.ErrSessionUnavailable
	}
	from, err := messaging.ParseURN(string(d.From))
	if err != nil {
		return teamhost.ErrInvalidRequest
	}
	queueRequest := store.NewRoutingReply{From: from, ParentID: "team-delivery:" + d.IdempotencyKey, Body: d.Body, TargetSessionID: d.Recipient.SessionID, Actor: string(d.From), IdempotencyKey: d.IdempotencyKey}
	prior, found, err := s.Store.PeekRoutingReply(ctx, queueRequest)
	if errors.Is(err, store.ErrRoutingReplyIdempotencyConflict) {
		return teams.ErrConflict
	}
	if err != nil {
		return err
	}
	if found {
		if prior.State == store.RoutingReplyUndeliverable {
			return teamhost.ErrSessionGone
		}
		return nil
	}
	row, err := s.Store.GetSession(d.Recipient.SessionID)
	if errors.Is(err, store.ErrSessionNotFound) {
		return teamhost.ErrSessionGone
	}
	if err != nil {
		return err
	}
	if session.State(row.State).Terminal() {
		return teamhost.ErrSessionGone
	}
	if row.State != string(session.StateRunning) {
		return teamhost.ErrSessionUnavailable
	}
	if dispatcher.rt.noTurnFeed != nil && dispatcher.rt.noTurnFeed(row.ID) {
		return teamhost.ErrInvalidRequest
	}
	request, err := json.Marshal(d)
	if err != nil {
		return err
	}
	reply, created, err := s.Store.CreateRoutingReply(ctx, store.NewRoutingReply{From: from, ParentID: "team-delivery:" + d.IdempotencyKey, Body: d.Body, TargetSessionID: row.ID, Actor: string(d.From), IdempotencyKey: d.IdempotencyKey, Metadata: map[string]string{"team_run": d.Route.RunID, "team_delivery_key": d.IdempotencyKey, "team_delivery_request": string(request)}})
	if errors.Is(err, store.ErrRoutingReplyIdempotencyConflict) {
		return teams.ErrConflict
	}
	if err != nil {
		return err
	}
	if reply.State == store.RoutingReplyUndeliverable {
		return teamhost.ErrSessionGone
	}
	if created {
		dispatcher.notifyNew(row.ID)
	}
	return nil
}

// CreateTeamSession retains the catalog logical agent for sandbox policy. The
// internal team marker skips ordinary actor bookkeeping: the enrollment adapter
// owns this separately enrolled member's exact actor binding.
func (s *Service) CreateTeamSession(ctx context.Context, key, launchID string) (*Launched, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := requestDigest(struct{ Op, Launch string }{"team-create", launchID})
	return s.createKeyed(key, digest, func() (*launch.Plan, error) {
		plan, err := s.BuildLaunchPlan(CreateSessionInput{LaunchID: launchID})
		if err != nil {
			return nil, err
		}
		plan.TeamMember = true
		return plan, nil
	})
}

// StopTeamSession shares launch serialization with ordinary session launches.
// It acknowledges only an observed terminal state, including a never-started
// keyed create. A detached/unavailable runtime is retryable, not already gone.
func (s *Service) StopTeamSession(ctx context.Context, id string) error {
	unlock, err := s.lockSessionLaunch(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	row, err := s.Store.GetSession(id)
	if errors.Is(err, store.ErrSessionNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if session.State(row.State).Terminal() {
		return nil
	}
	if row.State == string(session.StateCreated) {
		return s.Store.UpdateSessionState(id, string(session.StateKilled), 0, nil)
	}
	if err = s.StopSession(id); err != nil {
		if !errors.Is(err, agentsessions.ErrSessionNotRunning) {
			return err
		}
		row, err = s.Store.GetSession(id)
		if err != nil {
			return err
		}
		if session.State(row.State).Terminal() {
			return nil
		}
		return teamhost.ErrSessionUnavailable
	}
	_, waitErr := s.WaitSession(ctx, id)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	row, err = s.Store.GetSession(id)
	if err != nil {
		return err
	}
	if session.State(row.State).Terminal() {
		return nil
	}
	if waitErr != nil {
		return waitErr
	}
	return teamhost.ErrSessionUnavailable
}
