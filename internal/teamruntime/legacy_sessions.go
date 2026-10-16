package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/registry"
)

func (e *LegacyEnroller) LaunchTarget(ctx context.Context, key string, actor mesh.URN) (string, error) {
	r, err := e.read(ctx, "enrollment", key)
	if err != nil {
		return "", err
	}
	if r.ended != "" || r.bindingEnded {
		return "", teams.ErrDenied
	}
	var saved enrollmentReceipt
	if err = json.Unmarshal(r.payload, &saved); err != nil {
		return "", err
	}
	if saved.Enrollment.Actor != actor {
		return "", teams.ErrConflict
	}
	return saved.LaunchID, nil
}
func (e *LegacyEnroller) BindSession(ctx context.Context, key string, actor mesh.URN, id string) error {
	r, err := e.read(ctx, "enrollment", key)
	if err != nil {
		return err
	}
	// Replace only this key's reservation, atomically rechecking both fences.
	result, err := e.db.ExecContext(ctx, `UPDATE runtime_bindings SET session_id=?,updated_at=? WHERE host_id='team' AND visibility='tether-hosted' AND attempt_id=? AND target_urn=? AND revoked_at IS NULL AND EXISTS(SELECT 1 FROM team_port_intents WHERE port_kind='enrollment' AND intent_key=? AND ended='' AND binding_ended=0) AND EXISTS(SELECT 1 FROM team_port_intents WHERE port_kind='session' AND intent_key=? AND ended='')`, id, time.Now().UTC().Format(time.RFC3339Nano), r.bindingSecret, actor, key, key)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return teams.ErrUnavailable
	}
	return nil
}

// SessionActor answers only for the authenticated session supplied by Principals.
// It makes no identity-admission, verification or local-operator decision.
func (e *LegacyEnroller) SessionActor(ctx context.Context, id string) (mesh.URN, error) {
	var actor string
	err := e.db.QueryRowContext(ctx, `SELECT json_extract(request,'$.Actor') FROM team_port_intents WHERE port_kind='session' AND CAST(payload AS TEXT)=json_quote(?) AND ended='' AND state='done'`, id).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		var logical string
		err = e.db.QueryRowContext(ctx, `SELECT logical_agent_id FROM sessions WHERE id=?`, id).Scan(&logical)
		if errors.Is(err, sql.ErrNoRows) || err == nil && logical == "" {
			return "", teams.ErrNotFound
		}
		if err != nil {
			return "", err
		}
		actor = registry.LogicalAgentBindingTarget(logical)
	}
	if err != nil {
		return "", err
	}
	binding, err := e.registry.CurrentBinding(ctx, actor)
	if errors.Is(err, registry.ErrBindingNotFound) {
		return "", teams.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if binding.TargetURN != actor || binding.SessionID != id {
		return "", teams.ErrNotFound
	}
	return mesh.URN(actor), nil
}

var (
	_ SessionEnrollment = (*LegacyEnroller)(nil)
	_ SessionActors     = (*LegacyEnroller)(nil)
)

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

// AdoptionActor is what adoption needs to know about a registered actor.
type AdoptionActor struct {
	URN string
	// ActiveAgent is true only for an active (not deprecated or merged) agent.
	ActiveAgent bool
}

// AdoptionBinding is the actor's current registry binding as adoption reads it.
type AdoptionBinding struct {
	SessionID  string
	Generation int64
	// Live is true when the binding is unrevoked, unexpired and generation > 0.
	Live bool
}

// AdoptionActor reads an actor's registration. A missing actor is ErrNotFound.
func (e *LegacyEnroller) AdoptionActor(ctx context.Context, actor mesh.URN) (AdoptionActor, error) {
	profile, err := e.registry.Lookup(ctx, string(actor))
	if errors.Is(err, registry.ErrNotFound) {
		return AdoptionActor{}, teams.ErrNotFound
	}
	if err != nil {
		return AdoptionActor{}, err
	}
	return AdoptionActor{URN: profile.URN, ActiveAgent: profile.Kind == registry.KindAgent && profile.Status == registry.StatusActive}, nil
}

// AdoptionBinding reads an actor's current binding at now. No binding is a
// zero value, not an error: the actor simply is not bound.
func (e *LegacyEnroller) AdoptionBinding(ctx context.Context, actor mesh.URN, now time.Time) (AdoptionBinding, error) {
	binding, err := e.registry.CurrentBinding(ctx, string(actor))
	if errors.Is(err, registry.ErrBindingNotFound) {
		return AdoptionBinding{}, nil
	}
	if err != nil {
		return AdoptionBinding{}, err
	}
	live := binding.RevokedAt == nil && (binding.LeaseExpiresAt == nil || binding.LeaseExpiresAt.After(now)) && binding.Generation > 0
	return AdoptionBinding{SessionID: binding.SessionID, Generation: binding.Generation, Live: live}, nil
}
