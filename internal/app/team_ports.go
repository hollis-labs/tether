package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/mesh"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

// LinkTeamSession sets a retained team parent before the first launch. It is an
// internal adapter operation: the key must already own the created session.
func (s *Service) LinkTeamSession(ctx context.Context, id, key, parent string) error {
	result, err := s.Store.DB().ExecContext(ctx, `UPDATE sessions SET parent_session_id=NULLIF(?,'') WHERE id=? AND state='created' AND EXISTS(SELECT 1 FROM session_idempotency WHERE principal_id=? AND key=? AND session_id=?)`, parent, id, store.IdempotencyPrincipalTeam, key, id)
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
	record, err := s.Store.GetSessionIdempotency(store.IdempotencyPrincipalTeam, key)
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
	if d.Recipient.SessionID == "" && (d.Recipient.Kind != mesh.ActorAgent || d.Recipient.Governance == teams.Owner) {
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
	body := teamDeliveryBody(d)
	queueRequest := store.NewRoutingReply{From: from, ParentID: "team-delivery:" + d.IdempotencyKey, Body: body, TargetSessionID: d.Recipient.SessionID, Actor: string(d.From), IdempotencyKey: d.IdempotencyKey}
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
	reply, created, err := s.Store.CreateRoutingReply(ctx, store.NewRoutingReply{From: from, ParentID: "team-delivery:" + d.IdempotencyKey, Body: body, TargetSessionID: row.ID, Actor: string(d.From), IdempotencyKey: d.IdempotencyKey, Metadata: map[string]string{"team_run": d.Route.RunID, "team_delivery_key": d.IdempotencyKey, "team_delivery_request": string(request)}})
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

// teamDeliveryBody prefixes a delivered team message with who sent it and how
// to answer: the coordinator's address (where replies and results land as
// mail) and, for a delegation, the key tether_team_report_result needs.
func teamDeliveryBody(d teams.Delivery) string {
	if d.Verb == mesh.Reply || d.Route.Sender.Actor == "" {
		return d.Body
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[team %s: you are @%s (%s); message from %s; coordinator %s. ", d.Route.RunID, d.Recipient.Slot, d.Recipient.Actor, d.From, d.Route.Sender.Actor)
	fmt.Fprintf(&b, "Reply by mail with tether_message_notify {from: %q, to: %q, kind: \"response\"}", d.Recipient.Actor, d.Route.Sender.Actor)
	if d.Verb == mesh.Delegate {
		fmt.Fprintf(&b, ", or finish with tether_team_report_result {run_id: %q, delegate_key: %q, body: <result>}", d.Route.RunID, d.IdempotencyKey)
	}
	b.WriteString(".]\n\n")
	b.WriteString(d.Body)
	return b.String()
}

// CreateTeamSession retains the catalog logical agent for sandbox policy. The
// internal team marker skips ordinary actor bookkeeping: the enrollment adapter
// owns this separately enrolled member's exact actor binding.
func (s *Service) CreateTeamSession(ctx context.Context, key, launchID string) (*Launched, error) {
	return s.CreateTeamSessionWithContext(ctx, key, launchID, TeamBootContext{})
}

// CreateTeamSessionWithContext seals accepted non-secret slot context in the
// same immutable keyed create as its launch plan.
func (s *Service) CreateTeamSessionWithContext(ctx context.Context, key, launchID string, boot TeamBootContext) (*Launched, error) {
	return s.CreateTeamSessionWithLimits(ctx, key, launchID, boot, 0)
}

// CreateTeamSessionWithLimits additionally enforces the member's spawn timeout
// at launch: it becomes the plan's lifecycle max_duration (the tighter of it and
// any catalog max_duration), which the session reaper stops the session at.
// maxDuration 0 leaves the plan and the idempotency digest unchanged.
func (s *Service) CreateTeamSessionWithLimits(ctx context.Context, key, launchID string, boot TeamBootContext, maxDuration time.Duration) (*Launched, error) {
	if maxDuration < 0 {
		return nil, teamhost.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := boot.validate(); err != nil {
		return nil, err
	}
	digest := teamCreateDigest(launchID, boot)
	if maxDuration > 0 {
		digest = requestDigest(struct{ Base, MaxDuration string }{digest, maxDuration.String()})
	}
	mutate := func(plan *launch.Plan) error {
		plan.TeamMember = true
		// Each keyed team create gets its own logical id; sandbox and launch
		// policy keep resolving from the catalog agent.
		sum := sha256.Sum256([]byte(key))
		plan.CatalogAgentID = plan.LogicalAgentID
		plan.LogicalAgentID += "#" + hex.EncodeToString(sum[:6])
		if err := s.Store.UpsertLogicalAgent(agent.LogicalAgent{ID: plan.LogicalAgentID, Name: plan.LogicalAgentID, Role: "team member of " + plan.CatalogAgentID}, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		capTeamMaxDuration(plan, maxDuration)
		return applyTeamBootContext(plan, boot)
	}
	// A launch that names a worker places its team members there; it never
	// falls back to the hub (createWorkerSessionWith fails closed when the
	// worker is unknown or offline). No worker configured = hub, unchanged.
	if w := s.launchWorker(launchID); w != "" {
		return s.createWorkerSessionWith(CreateSessionInput{LaunchID: launchID, Worker: w, IdempotencyKey: key, IdempotencyPrincipal: store.IdempotencyPrincipalTeam}, digest, mutate)
	}
	return s.createKeyed(store.IdempotencyPrincipalTeam, key, digest, func() (*launch.Plan, error) {
		plan, err := s.BuildLaunchPlan(CreateSessionInput{LaunchID: launchID})
		if err != nil {
			return nil, err
		}
		return plan, mutate(plan)
	})
}

// launchWorker returns the catalog launch's `worker:` default placement.
func (s *Service) launchWorker(launchID string) string {
	if c := s.cat(); c != nil {
		return c.Launches[launchID].Worker
	}
	return ""
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
	if row.WorkerID != "" {
		return s.stopWorkerTeamSession(row)
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

// stopWorkerTeamSession stops a worker-placed member by session id whatever
// state the hub recorded: a hub-side failed/launching row can still have a live
// process on the worker (the launch acknowledgement may have been lost), so a
// terminal row is not proof the process is gone. An unreachable worker leaves
// a live-looking row retryable; "unknown execution" means the worker holds
// nothing to stop.
func (s *Service) stopWorkerTeamSession(row *store.SessionRow) error {
	terminal := session.State(row.State).Terminal()
	err := s.StopSession(row.ID)
	switch {
	case err == nil, strings.Contains(fmt.Sprint(err), "unknown execution"):
	case terminal:
		log.Printf("team stop: worker session %s already %s on the hub; worker stop failed: %v", row.ID, row.State, err)
		return nil
	default:
		return fmt.Errorf("%w: stop worker session %s: %w", teamhost.ErrSessionUnavailable, row.ID, err)
	}
	if !terminal {
		if err = s.Store.UpdateSessionState(row.ID, string(session.StateKilled), 0, nil); err != nil {
			return err
		}
		s.releaseWorkerIdentity(row.ID)
		if s.Registry != nil {
			_, _ = s.Registry.RevokeSessionBindings(context.Background(), row.ID)
		}
	}
	return nil
}

// capTeamMaxDuration lowers the plan's lifecycle max_duration to d; a catalog
// value that is already tighter wins, and d == 0 changes nothing.
func capTeamMaxDuration(plan *launch.Plan, d time.Duration) {
	if d <= 0 {
		return
	}
	var base launchprofile.LifecycleDurations
	if plan.Lifecycle != nil {
		base, _ = plan.Lifecycle.Durations()
	}
	if base.MaxDuration > 0 && base.MaxDuration <= d {
		return
	}
	v := d.String()
	merged := launchprofile.MergeLifecycle(plan.Lifecycle, &launchprofile.LifecyclePolicy{MaxDuration: &v})
	plan.Lifecycle = merged
}
