package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
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
