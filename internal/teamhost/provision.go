package teamhost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

type intent struct {
	request                  teams.ProvisionRequest
	enrollment               *Enrollment
	member                   *teams.Member
	parentSession, tombstone string
	cleaned                  bool
}

func loadIntent(ctx context.Context, q rowReader, key string) (intent, error) {
	var out intent
	var req, enrollment, member []byte
	err := q.QueryRowContext(ctx, `SELECT request,enrollment,member,parent_session,tombstone,cleaned FROM team_host_intents WHERE intent_key=?`, key).Scan(&req, &enrollment, &member, &out.parentSession, &out.tombstone, &out.cleaned)
	if err != nil {
		return out, notFound(err)
	}
	if err = decode(req, &out.request); err != nil {
		return out, err
	}
	if len(enrollment) > 0 {
		var e Enrollment
		if err = decode(enrollment, &e); err != nil {
			return out, err
		}
		out.enrollment = &e
	}
	if len(member) > 0 {
		var m teams.Member
		if err = decode(member, &m); err != nil {
			return out, err
		}
		out.member = &m
	}
	return out, nil
}

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (h *Host) reserve(ctx context.Context, req teams.ProvisionRequest) (intent, error) {
	var result intent
	payload, err := encode(req)
	if err != nil {
		return result, err
	}
	err = h.tx(ctx, func(conn *sql.Conn) error {
		old, err := loadIntent(ctx, conn, req.IdempotencyKey)
		if err == nil {
			if old.tombstone != "" {
				return teams.ErrDenied
			}
			oldPayload, err := encode(old.request)
			if err != nil {
				return err
			}
			equal, err := same(oldPayload, req)
			if err != nil {
				return err
			}
			if !equal {
				return teams.ErrConflict
			}
			result = old
			return nil
		}
		if !errors.Is(err, teams.ErrNotFound) {
			return err
		}
		parent := ""
		if req.Parent != "" {
			var payload []byte
			if err := conn.QueryRowContext(ctx, `SELECT payload FROM team_rosters WHERE run_id=?`, req.RunID).Scan(&payload); err != nil {
				return notFound(err)
			}
			var roster teams.Roster
			if err := decode(payload, &roster); err != nil {
				return err
			}
			for _, m := range roster.Members {
				if m.ID == req.Parent && m.Status == "active" {
					parent = m.SessionID
				}
			}
			if parent == "" {
				return teams.ErrUnavailable
			}
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_intents(intent_key,request,parent_session) VALUES(?,?,?)`, req.IdempotencyKey, payload, parent)
		if err == nil {
			result.request = req
			result.parentSession = parent
		}
		return err
	})
	return result, err
}

// requestTeam recovers root provenance from the persisted launch intent, or
// resolves a spawned member's immutable run definition. Authority is explicit.
func (h *Host) requestTeam(ctx context.Context, req teams.ProvisionRequest) error {
	var team teams.Team
	if req.RunID != "" {
		run, err := h.store.GetRun(ctx, req.RunID)
		if err != nil {
			return err
		}
		if req.Slot.Resolution == teams.Pool {
			roster, rosterErr := h.store.Snapshot(ctx, req.RunID)
			if rosterErr != nil && !errors.Is(rosterErr, teams.ErrNotFound) {
				return rosterErr
			}
			if subset, present := roster.PoolIdentities[req.Slot.Name]; present {
				admitted := false
				for _, actor := range subset {
					if actor == req.Identity {
						admitted = true
					}
				}
				if !admitted {
					return teams.ErrProvisionFailed
				}
			}
		}
		team, err = h.store.GetDefinition(ctx, run.Run.TeamID, run.Run.TeamVersion)
		if err != nil {
			return err
		}
	} else {
		var payload []byte
		err := h.db.QueryRowContext(ctx, `SELECT l.payload FROM team_host_launch_intents i JOIN team_launches l USING(launch_key) WHERE i.intent_key=?`, req.IdempotencyKey).Scan(&payload)
		if err != nil {
			return fmt.Errorf("missing launch lookup for intent %q: %w", req.IdempotencyKey, notFound(err))
		}
		var record teams.LaunchRecord
		if err = decode(payload, &record); err != nil {
			return err
		}
		team = record.Team
		matched := false
		for _, intent := range record.Intents {
			if intent.Key == req.IdempotencyKey {
				b, err := encode(intent.Request)
				if err != nil {
					return err
				}
				matched, err = same(b, req)
				if err != nil {
					return err
				}
			}
		}
		if !matched {
			return teams.ErrProvisionFailed
		}
	}
	if team.Authority.Mode != teams.Strict {
		return teams.ErrProvisionFailed
	}
	if err := teams.Validate(team); err != nil {
		return fmt.Errorf("%w: %w", teams.ErrProvisionFailed, err)
	}
	found := false
	for _, slot := range team.Slots {
		if slot.Name == req.Slot.Name && slot.Definition == req.Slot.Definition && slot.Resolution == req.Slot.Resolution {
			if slot.Resolution == teams.Durable && req.Identity != slot.Identity {
				return teams.ErrProvisionFailed
			}
			if slot.Resolution == teams.Pool {
				declared := false
				for _, actor := range slot.Identities {
					if actor == req.Identity {
						declared = true
					}
				}
				if !declared {
					return teams.ErrProvisionFailed
				}
			}
			if slot.Resolution == teams.Fresh && req.Identity != "" {
				return teams.ErrProvisionFailed
			}
			found = true
		}
	}
	if !found {
		return teams.ErrProvisionFailed
	}
	return nil
}

// Provision persists intent before external calls. Every port call is keyed and
// can be retried after an acknowledgement loss. A tombstone never resurrects.
func (h *Host) Provision(ctx context.Context, req teams.ProvisionRequest) (teams.Member, error) {
	if req.IdempotencyKey == "" || req.MemberID == "" || req.Slot.Name == "" {
		return teams.Member{}, fmt.Errorf("%w: pinned intent required", teams.ErrProvisionFailed)
	}
	if req.Slot.Resolution == teams.Fresh && (req.Slot.Definition.ID == "" || req.Slot.Definition.Revision == "") {
		return teams.Member{}, teams.ErrProvisionFailed
	}
	if err := req.Limits.Validate(); err != nil {
		return teams.Member{}, fmt.Errorf("%w: %w", teams.ErrProvisionFailed, err)
	}
	if req.Slot.Resolution != teams.Fresh && req.Slot.Resolution != teams.Pool && req.Slot.Resolution != teams.Durable {
		return teams.Member{}, teams.ErrProvisionFailed
	}
	if req.Slot.Resolution != teams.Fresh && req.Identity == "" {
		return teams.Member{}, teams.ErrProvisionFailed
	}
	if err := h.requestTeam(ctx, req); err != nil {
		return teams.Member{}, err
	}
	state, err := h.reserve(ctx, req)
	if err != nil {
		return teams.Member{}, err
	}
	if state.member != nil {
		return *state.member, nil
	}
	actor := req.Identity
	if req.Slot.Resolution == teams.Fresh {
		actor = mesh.URN("msg://agent/team/" + id("enrollment", req.IdempotencyKey))
	}
	for _, reserved := range req.ReservedIdentities {
		if req.Slot.Resolution == teams.Fresh && actor == reserved {
			return teams.Member{}, teams.ErrProvisionFailed
		}
	}
	enrollment, err := h.ports.Enroller.Ensure(ctx, EnrollmentRequest{IntentKey: req.IdempotencyKey, Actor: actor, Provision: req})
	if err != nil {
		return teams.Member{}, h.compensateIfEnded(ctx, req.IdempotencyKey, err)
	}
	if enrollment.Actor != actor || enrollment.AgentID == "" || enrollment.Kind != mesh.ActorAgent || enrollment.Ephemeral != (req.Slot.Resolution == teams.Fresh) {
		return teams.Member{}, fmt.Errorf("%w: invalid enrollment", teams.ErrProvisionFailed)
	}
	payload, err := encode(enrollment)
	if err != nil {
		return teams.Member{}, err
	}
	err = h.tx(ctx, func(conn *sql.Conn) error {
		current, err := loadIntent(ctx, conn, req.IdempotencyKey)
		if err != nil {
			return err
		}
		if current.tombstone != "" {
			return teams.ErrDenied
		}
		if current.enrollment != nil {
			old, err := encode(current.enrollment)
			if err != nil {
				return err
			}
			equal, err := same(old, enrollment)
			if err != nil {
				return err
			}
			if !equal {
				return teams.ErrConflict
			}
		}
		var owner string
		err = conn.QueryRowContext(ctx, `SELECT intent_key FROM team_host_bindings WHERE actor=?`, actor).Scan(&owner)
		if err == nil && owner != req.IdempotencyKey {
			holder, loadErr := loadIntent(ctx, conn, owner)
			if loadErr != nil {
				return loadErr
			}
			if holder.tombstone != "" && !holder.cleaned {
				return teams.ErrUnavailable
			}
			return teams.ErrProvisionFailed
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_bindings(actor,intent_key) VALUES(?,?) ON CONFLICT(actor) DO NOTHING`, actor, req.IdempotencyKey)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE team_host_intents SET enrollment=? WHERE intent_key=?`, payload, req.IdempotencyKey)
		return err
	})
	if err != nil {
		return teams.Member{}, h.compensateIfEnded(ctx, req.IdempotencyKey, err)
	}
	if err = h.ports.Enroller.AcquireBinding(ctx, req.IdempotencyKey, actor); err != nil {
		return teams.Member{}, h.compensateIfEnded(ctx, req.IdempotencyKey, err)
	}
	// A keyed Stop must fence a Launch that races this observation.
	if err = h.live(ctx, req.IdempotencyKey); err != nil {
		return teams.Member{}, h.compensateIfEnded(ctx, req.IdempotencyKey, err)
	}
	session, err := h.ports.Sessions.Launch(ctx, SessionRequest{IntentKey: req.IdempotencyKey, Actor: actor, ParentSession: state.parentSession, Provision: req})
	if err != nil {
		return teams.Member{}, h.compensateIfEnded(ctx, req.IdempotencyKey, err)
	}
	if session == "" {
		return teams.Member{}, teams.ErrProvisionFailed
	}
	member := teams.Member{ID: req.MemberID, Slot: req.Slot.Name, Actor: actor, Kind: enrollment.Kind, AgentID: enrollment.AgentID, SessionID: session, Status: "active", Governance: teams.MemberRole, Resolution: req.Slot.Resolution, Parent: req.Parent, Enrolled: true, Ephemeral: enrollment.Ephemeral, SpawnCapable: enrollment.SpawnCapable, Intent: &req, Budget: req.Limits.Budget, Limits: req.Limits, Idle: true, JoinedAt: h.Now()}
	err = h.tx(ctx, func(conn *sql.Conn) error {
		current, err := loadIntent(ctx, conn, req.IdempotencyKey)
		if err != nil {
			return err
		}
		if current.tombstone != "" {
			return teams.ErrDenied
		}
		if current.member != nil {
			member = *current.member
			return nil
		}
		payload, err := encode(member)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE team_host_intents SET member=? WHERE intent_key=? AND tombstone=''`, payload, req.IdempotencyKey)
		return err
	})
	if err != nil {
		return teams.Member{}, h.compensateIfEnded(ctx, req.IdempotencyKey, err)
	}
	return member, nil
}
func (h *Host) live(ctx context.Context, key string) error {
	state, err := loadIntent(ctx, h.db, key)
	if err != nil {
		return err
	}
	if state.tombstone != "" {
		return teams.ErrDenied
	}
	return nil
}
func (h *Host) compensateIfEnded(ctx context.Context, key string, cause error) error {
	cleanup := context.WithoutCancel(ctx)
	state, err := loadIntent(cleanup, h.db, key)
	if err == nil && state.tombstone != "" {
		return errors.Join(cause, h.clean(cleanup, key, state.tombstone))
	}
	return cause
}
func (h *Host) Release(ctx context.Context, _ string, m teams.Member) error {
	return h.end(ctx, m, "release")
}
func (h *Host) Retire(ctx context.Context, _ string, m teams.Member) error {
	return h.end(ctx, m, "retire")
}
func (h *Host) end(ctx context.Context, m teams.Member, mode string) error {
	if m.Intent == nil {
		// A governance-only caller is not acquired by this host. Never infer
		// cleanup ownership from its actor identity or touch its resources.
		if m.ID == "" || m.Actor == "" || m.Governance != teams.Owner || m.SessionID != "" {
			return errors.New("cleanup: persisted intent required")
		}
		return h.tx(ctx, func(conn *sql.Conn) error {
			var acquired int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM team_host_intents WHERE json_extract(request,'$.MemberID')=? OR json_extract(member,'$.id')=?`, m.ID, m.ID).Scan(&acquired); err != nil {
				return err
			}
			if acquired != 0 {
				return errors.New("cleanup: persisted intent required")
			}
			return nil
		})
	}
	if m.Intent.IdempotencyKey == "" {
		return errors.New("cleanup: persisted intent required")
	}
	req := *m.Intent
	if (mode == "retire") != (req.Slot.Resolution == teams.Fresh) {
		return teams.ErrConflict
	}
	payload, err := encode(req)
	if err != nil {
		return err
	}
	err = h.tx(ctx, func(conn *sql.Conn) error {
		current, err := loadIntent(ctx, conn, req.IdempotencyKey)
		if err == nil {
			old, err := encode(current.request)
			if err != nil {
				return err
			}
			equal, err := same(old, req)
			if err != nil {
				return err
			}
			if !equal {
				return teams.ErrConflict
			}
			if current.tombstone != "" && current.tombstone != mode {
				return teams.ErrConflict
			}
			_, err = conn.ExecContext(ctx, `UPDATE team_host_intents SET tombstone=? WHERE intent_key=?`, mode, req.IdempotencyKey)
			return err
		}
		if !errors.Is(err, teams.ErrNotFound) {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_intents(intent_key,request,tombstone) VALUES(?,?,?)`, req.IdempotencyKey, payload, mode)
		return err
	})
	if err != nil {
		return err
	}
	return h.clean(ctx, req.IdempotencyKey, mode)
}
func (h *Host) clean(ctx context.Context, key, mode string) error {
	// Keep enrollment and binding until Stop acknowledges that the session is
	// gone. An uncertain Stop must never permit a second session for the actor.
	if err := h.ports.Sessions.Stop(ctx, key); err != nil && !errors.Is(err, teams.ErrNotFound) && !errors.Is(err, ErrSessionGone) {
		return err
	}
	binding := h.ports.Enroller.ReleaseBinding(ctx, key)
	var enrollment error
	if mode == "retire" {
		enrollment = h.ports.Enroller.Retire(ctx, key)
	} else {
		enrollment = h.ports.Enroller.Release(ctx, key)
	}
	if err := errors.Join(binding, enrollment); err != nil {
		return err
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `DELETE FROM team_host_bindings WHERE intent_key=?`, key)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE team_host_intents SET cleaned=1 WHERE intent_key=? AND tombstone=?`, key, mode)
		return err
	})
}

// ReconcileIntents repairs tombstoned cleanup that outlived its callback or
// crashed after a port side effect. The owner schedules it; no goroutine runs.
func (h *Host) ReconcileIntents(ctx context.Context, limit int) error {
	if limit < 1 {
		return errors.New("intent recovery: positive limit required")
	}
	entries, err := h.recoveryPage(ctx, "intents", limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, e := range entries {
		err := h.clean(ctx, e.key, e.mode)
		failures = append(failures, err, h.finishAttempt(ctx, "intents", e.key, err))
	}
	return errors.Join(failures...)
}
