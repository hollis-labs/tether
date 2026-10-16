package teamhost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
)

// Adopter is the seam that proves, and later lets go of, an adopted member.
// Verify reads the session's own records and mutates nothing; the request never
// supplies the launch, the pin or the owner. Release revokes the team
// enrollment and grants for one intent and sends the member one notice. It must
// not stop the session, release or rebind its registry binding, or touch its
// registration. Both are idempotent.
type Adopter interface {
	Verify(context.Context, AdoptionCheck) (Adoption, error)
	Release(ctx context.Context, intentKey string, member teams.Member) error
}

// AdoptionCheck names exactly what the caller claims to be adopting.
type AdoptionCheck struct {
	Actor     mesh.URN
	SessionID string
	// Principal is the forming principal; the binding must be owned by it.
	Principal mesh.URN
}

// Adoption is the verified state of an adoptable actor and its session.
type Adoption struct {
	Actor   mesh.URN
	AgentID string
	Kind    mesh.ActorKind
	// SessionID is the session the binding resolves to.
	SessionID string
	// Generation is the registry binding generation the session runs under.
	Generation uint64
	// Resolved is false for an observed or unverified binding.
	Resolved bool
	// Owner is the principal that owns the binding.
	Owner mesh.URN
	// LaunchID and Pin come from the session's launch record.
	LaunchID string
	Pin      mesh.DefinitionRef
}

// AdoptRequest joins a live actor and session to a run. Provision carries the
// run, the member id and the idempotency key; the host fixes its resolution.
type AdoptRequest struct {
	Provision         teams.ProvisionRequest
	SessionID         string
	BindingGeneration uint64
	Principal         mesh.URN
}

// AdoptedLaunches allowlists the launches whose sessions may be adopted, each
// with the one definition pin it must run.
type AdoptedLaunches map[string]mesh.DefinitionRef

// Adopt records an already-running actor as a team member. Everything is
// derived from Adopter.Verify; a mismatch with the request is refused. The
// actor and the session may belong to at most one active team (a provisioned
// member holds the same binding row), and a losing race is refused, not queued.
func (h *Host) Adopt(ctx context.Context, req AdoptRequest) (teams.Member, error) {
	p := req.Provision
	if h.ports.Adopter == nil {
		return teams.Member{}, teams.ErrUnavailable
	}
	if p.IdempotencyKey == "" || p.MemberID == "" || p.RunID == "" || p.Slot.Name == "" || p.Identity.Validate() != nil || req.SessionID == "" || req.BindingGeneration == 0 || req.Principal.Validate() != nil {
		return teams.Member{}, fmt.Errorf("%w: adoption names actor, session, binding generation and principal", teams.ErrProvisionFailed)
	}
	if p.Slot.Definition.ID == "" || p.Slot.Definition.Revision == "" || p.Slot.Definition.Digest == "" {
		return teams.Member{}, fmt.Errorf("%w: exact definition pin required", teams.ErrProvisionFailed)
	}
	if err := p.Limits.Validate(); err != nil {
		return teams.Member{}, fmt.Errorf("%w: %w", teams.ErrProvisionFailed, err)
	}
	// An adopted member holds a durable identity it did not get from this run.
	p.Slot.Resolution = teams.Durable
	if err := h.adoptableSlot(ctx, p); err != nil {
		return teams.Member{}, err
	}
	state, err := h.reserve(ctx, p)
	if err != nil {
		return teams.Member{}, fmt.Errorf("member %s (slot %s): reserve adoption: %w", p.MemberID, p.Slot.Name, err)
	}
	if state.member != nil {
		// A replay must be the same adoption, not the same key with other facts.
		var session, principal string
		var generation uint64
		if err = h.db.QueryRowContext(ctx, `SELECT session_id,principal,binding_generation FROM team_host_adoptions WHERE intent_key=?`, p.IdempotencyKey).Scan(&session, &principal, &generation); err != nil {
			return teams.Member{}, notFound(err)
		}
		if session != req.SessionID || principal != string(req.Principal) || generation != req.BindingGeneration {
			return teams.Member{}, teams.ErrConflict
		}
		return *state.member, nil
	}
	verified, err := h.ports.Adopter.Verify(ctx, AdoptionCheck{Actor: p.Identity, SessionID: req.SessionID, Principal: req.Principal})
	if err != nil {
		return teams.Member{}, h.compensateAdoption(ctx, p.IdempotencyKey, fmt.Errorf("member %s (slot %s): verify adoption: %w", p.MemberID, p.Slot.Name, err))
	}
	if err = h.checkAdoption(verified, req, p); err != nil {
		return teams.Member{}, h.compensateAdoption(ctx, p.IdempotencyKey, err)
	}
	member := teams.Member{ID: p.MemberID, Slot: p.Slot.Name, Actor: verified.Actor, Kind: verified.Kind, AgentID: verified.AgentID, SessionID: verified.SessionID, Status: "active", Governance: teams.MemberRole, Resolution: teams.Durable, Parent: p.Parent, Enrolled: true, SpawnCapable: false, Intent: &p, Budget: p.Limits.Budget, Limits: p.Limits, Idle: true, JoinedAt: h.Now()}
	memberJSON, err := encode(member)
	if err != nil {
		return teams.Member{}, err
	}
	enrollment := Enrollment{Actor: verified.Actor, AgentID: verified.AgentID, Kind: verified.Kind}
	enrollmentJSON, err := encode(enrollment)
	if err != nil {
		return teams.Member{}, err
	}
	err = h.tx(ctx, func(conn *sql.Conn) error {
		current, err := loadIntent(ctx, conn, p.IdempotencyKey)
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
		// One active team per actor, provisioned or adopted: the binding row is
		// the compare-and-set. A holder that is itself being cleaned up is not
		// yet free.
		var owner string
		err = conn.QueryRowContext(ctx, `SELECT intent_key FROM team_host_bindings WHERE actor=?`, verified.Actor).Scan(&owner)
		if err == nil && owner != p.IdempotencyKey {
			return teams.ErrConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO team_host_bindings(actor,intent_key) VALUES(?,?) ON CONFLICT(actor) DO NOTHING`, verified.Actor, p.IdempotencyKey); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_adoptions(intent_key,run_id,actor,session_id,binding_generation,launch_id,definition_id,definition_revision,definition_digest,principal) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			p.IdempotencyKey, p.RunID, verified.Actor, verified.SessionID, verified.Generation, verified.LaunchID, verified.Pin.ID, verified.Pin.Revision, verified.Pin.Digest, req.Principal)
		if err != nil {
			if isUnique(err) {
				return teams.ErrConflict
			}
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE team_host_intents SET enrollment=?,member=? WHERE intent_key=? AND tombstone=''`, enrollmentJSON, memberJSON, p.IdempotencyKey)
		return err
	})
	if err != nil {
		return teams.Member{}, h.compensateAdoption(ctx, p.IdempotencyKey, err)
	}
	return member, nil
}

func (h *Host) checkAdoption(v Adoption, req AdoptRequest, p teams.ProvisionRequest) error {
	switch {
	case v.Actor != p.Identity || v.Kind != mesh.ActorAgent || v.AgentID == "":
		return fmt.Errorf("%w: actor is not an enrolled agent", teams.ErrProvisionFailed)
	case !v.Resolved:
		return fmt.Errorf("%w: binding is not resolved", teams.ErrProvisionFailed)
	case v.SessionID != req.SessionID:
		return fmt.Errorf("%w: binding resolves to another session", teams.ErrConflict)
	case v.Generation != req.BindingGeneration:
		return fmt.Errorf("%w: binding generation changed", teams.ErrConflict)
	case v.Owner != req.Principal:
		return fmt.Errorf("%w: binding is not owned by the forming principal", teams.ErrDenied)
	}
	pin, allowed := h.adoptable[v.LaunchID]
	if !allowed || v.LaunchID == "" {
		return fmt.Errorf("%w: launch %q is not adoptable", teams.ErrDenied, v.LaunchID)
	}
	if pin != v.Pin || v.Pin != p.Slot.Definition {
		return fmt.Errorf("%w: definition pin does not match the session's launch", teams.ErrDenied)
	}
	return nil
}

// adoptableSlot checks the slot against the run's immutable definition: the
// exact pin, and a resolution that already names the adopted identity. A fresh
// slot mints its own ephemeral identity, so an existing actor cannot fill it;
// a pool or durable slot must declare the actor.
func (h *Host) adoptableSlot(ctx context.Context, req teams.ProvisionRequest) error {
	run, err := h.store.GetRun(ctx, req.RunID)
	if err != nil {
		return err
	}
	team, err := h.store.GetDefinition(ctx, run.Run.TeamID, run.Run.TeamVersion)
	if err != nil {
		return err
	}
	if team.Authority.Mode != teams.Strict {
		return teams.ErrProvisionFailed
	}
	for _, slot := range team.Slots {
		if slot.Name != req.Slot.Name || slot.Definition != req.Slot.Definition {
			continue
		}
		switch slot.Resolution {
		case teams.Durable:
			if slot.Identity == req.Identity {
				return nil
			}
		case teams.Pool:
			for _, actor := range slot.Identities {
				if actor == req.Identity {
					return nil
				}
			}
		case teams.Fresh:
			// A fresh slot mints its own identity; an existing actor cannot fill it.
		}
		return fmt.Errorf("%w: slot %q does not declare %s", teams.ErrProvisionFailed, slot.Name, req.Identity)
	}
	return teams.ErrProvisionFailed
}

// compensateAdoption fences a failed adoption: nothing was started, so the
// intent is tombstoned and cleaned without touching any port.
func (h *Host) compensateAdoption(ctx context.Context, key string, cause error) error {
	err := h.tx(context.WithoutCancel(ctx), func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM team_host_bindings WHERE intent_key=?`, key); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE team_host_intents SET tombstone='release',cleaned=1 WHERE intent_key=? AND tombstone='' AND member IS NULL`, key)
		return err
	})
	return errors.Join(cause, err)
}

// isAdopted reports whether the intent was recorded as an adoption.
func (h *Host) isAdopted(ctx context.Context, key string) (bool, error) {
	var adopted bool
	err := h.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_adoptions WHERE intent_key=?)`, key).Scan(&adopted)
	return adopted, err
}

// cleanAdopted releases an adopted member. It never stops the session, never
// releases the registry binding and never touches the registration: the member
// loses only its team enrollment and grants.
func (h *Host) cleanAdopted(ctx context.Context, key, mode string) error {
	if h.ports.Adopter == nil {
		// Fail closed: never fall through to the provisioned path, which stops.
		return teams.ErrUnavailable
	}
	current, err := loadIntent(ctx, h.db, key)
	if err != nil {
		return err
	}
	if current.member != nil {
		if err = h.ports.Adopter.Release(ctx, key, *current.member); err != nil {
			return err
		}
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM team_host_bindings WHERE intent_key=?`, key); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE team_host_adoptions SET released=1 WHERE intent_key=?`, key); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE team_host_intents SET cleaned=1 WHERE intent_key=? AND tombstone=?`, key, mode)
		return err
	})
}

// SuspendRebound suspends every active adoption whose registry binding is no
// longer the generation that was adopted. A suspended member receives no
// messages until it is explicitly re-adopted. The owner schedules it.
func (h *Host) SuspendRebound(ctx context.Context, limit int) error {
	if limit < 1 {
		return errors.New("adoption check: positive limit required")
	}
	if h.ports.Adopter == nil {
		return nil
	}
	rows, err := h.db.QueryContext(ctx, `SELECT intent_key,actor,session_id,binding_generation,principal FROM team_host_adoptions WHERE released=0 AND suspended=0 ORDER BY intent_key LIMIT ?`, limit)
	if err != nil {
		return err
	}
	type entry struct {
		key, actor, session, principal string
		generation                     uint64
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.key, &e.actor, &e.session, &e.generation, &e.principal); err != nil {
			_ = rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var failures []error
	for _, e := range entries {
		v, verr := h.ports.Adopter.Verify(ctx, AdoptionCheck{Actor: mesh.URN(e.actor), SessionID: e.session, Principal: mesh.URN(e.principal)})
		if verr == nil && v.Resolved && v.SessionID == e.session && v.Generation == e.generation {
			continue
		}
		if verr != nil && !errors.Is(verr, teams.ErrDenied) && !errors.Is(verr, teams.ErrConflict) && !errors.Is(verr, teams.ErrNotFound) && !errors.Is(verr, teams.ErrProvisionFailed) {
			failures = append(failures, verr)
			continue
		}
		_, uerr := h.db.ExecContext(ctx, `UPDATE team_host_adoptions SET suspended=1 WHERE intent_key=? AND released=0`, e.key)
		failures = append(failures, uerr)
	}
	return errors.Join(failures...)
}

// Readopt explicitly resumes a suspended adoption under the binding's current
// generation, after the same verification as the first adoption.
func (h *Host) Readopt(ctx context.Context, key string, generation uint64) error {
	if h.ports.Adopter == nil {
		return teams.ErrUnavailable
	}
	if generation == 0 {
		return teams.ErrProvisionFailed
	}
	var actor, session, principal string
	var launch string
	var pin mesh.DefinitionRef
	err := h.db.QueryRowContext(ctx, `SELECT actor,session_id,principal,launch_id,definition_id,definition_revision,definition_digest FROM team_host_adoptions WHERE intent_key=? AND released=0`, key).Scan(&actor, &session, &principal, &launch, &pin.ID, &pin.Revision, &pin.Digest)
	if err != nil {
		return notFound(err)
	}
	v, err := h.ports.Adopter.Verify(ctx, AdoptionCheck{Actor: mesh.URN(actor), SessionID: session, Principal: mesh.URN(principal)})
	if err != nil {
		return err
	}
	if !v.Resolved || v.Actor != mesh.URN(actor) || v.SessionID != session || v.Owner != mesh.URN(principal) || v.Generation != generation || v.LaunchID != launch || v.Pin != pin {
		return teams.ErrConflict
	}
	if allowed, ok := h.adoptable[v.LaunchID]; !ok || allowed != v.Pin {
		return teams.ErrDenied
	}
	res, err := h.db.ExecContext(ctx, `UPDATE team_host_adoptions SET binding_generation=?,suspended=0 WHERE intent_key=? AND released=0`, generation, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return teams.ErrNotFound
	}
	return nil
}

// adoptionSuspended reports whether the actor's active adoption is suspended.
func (h *Host) adoptionSuspended(ctx context.Context, actor mesh.URN, session string) (bool, error) {
	var suspended bool
	err := h.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_adoptions WHERE actor=? AND session_id=? AND released=0 AND suspended=1)`, actor, session).Scan(&suspended)
	return suspended, err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
