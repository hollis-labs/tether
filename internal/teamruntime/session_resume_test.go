package teamruntime

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

type recoveryEffects struct {
	*sessionEffects
	recovered string
	resume    func(context.Context, string) error
}

func (e *recoveryEffects) RecoverTeamSession(ctx context.Context, _ string, id string) error {
	e.recovered = id
	if e.resume != nil {
		return e.resume(ctx, id)
	}
	return nil
}

func TestRetainedSessionPortRetriesRemappedDestinationAndFencesStaleReceipt(t *testing.T) {
	for _, stopRace := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed_pending", true: "stop_race"}[stopRace], func(t *testing.T) {
			db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "state.db"))
			port, e, effects, req := sessionFixture(t, db, s, reg, "replacement")
			old, err := port.Launch(ctx, req)
			check(t, err)
			binding, err := reg.CurrentBinding(ctx, string(req.Actor))
			check(t, err)
			id := "replacement-destination"
			check(t, db.CreateSession(store.SessionRow{ID: id, State: "created", ParentSessionID: sql.NullString{String: old, Valid: true}}, &launch.Plan{}))
			// Seed only a committed transition for this adapter test. The real
			// replacement transaction/authority/rollback is exercised by app.
			check(t, s.WithTransaction(ctx, func(conn *sql.Conn) error {
				for _, statement := range []struct {
					q    string
					args []any
				}{
					{`UPDATE runtime_bindings SET session_id=? WHERE id=?`, []any{id, binding.ID}},
					{`UPDATE team_port_intents SET payload=json_quote(?) WHERE port_kind='session' AND intent_key=?`, []any{id, req.IntentKey}},
					{`UPDATE session_idempotency SET session_id=? WHERE session_id=?`, []any{id, old}},
					{`INSERT INTO session_replacements(source_session_id,replacement_session_id,actor_uri,intent_key,binding_id,binding_generation,source_plan_digest,credential_scopes_json,credential_mode,committed_at) VALUES(?,?,?,?,?,?,'fixture','[]','off','fixture')`, []any{old, id, string(req.Actor), req.IntentKey, binding.ID, binding.Generation}},
				} {
					if _, err := conn.ExecContext(ctx, statement.q, statement.args...); err != nil {
						return err
					}
				}
				return nil
			}))
			recovery := &recoveryEffects{sessionEffects: effects, resume: func(callCtx context.Context, currentID string) error {
				if stopRace {
					return port.end(callCtx, "session", req.IntentKey, "stop", false)
				}
				_, err := effects.LaunchSessionWithContext(callCtx, currentID)
				return err
			}}
			port, err = NewSessions(db, s, recovery, e)
			check(t, err)
			if err := port.Recover(ctx, req.IntentKey, old); !errors.Is(err, teams.ErrConflict) || recovery.recovered != "" {
				t.Fatal("stale receipt invoked old recovery", err)
			}
			effects.beforeCreate = func() { t.Fatal("retry re-ran original provisioning") }
			current, err := port.Launch(ctx, req)
			if stopRace {
				if !errors.Is(err, teams.ErrDenied) {
					t.Fatal("stop racing remapped recovery admitted", err)
				}
			} else {
				check(t, err)
			}
			if current != id || recovery.recovered != id {
				t.Fatal("receipt did not select committed destination")
			}
			row, err := db.GetSession(id)
			check(t, err)
			wantState := "running"
			if stopRace {
				wantState = "killed"
			}
			if row.State != wantState || row.ParentSessionID.String != old {
				t.Fatal("retry changed lineage or stopped wrong session", row.State)
			}
		})
	}
}

func TestRetainedSessionRecoveryKeepsOriginalFreshBindingAndStopFence(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "state.db"))
	port, e, effects, req := sessionFixture(t, db, s, reg, "recovery")
	id, err := port.Launch(ctx, req)
	check(t, err)
	binding, err := reg.CurrentBinding(ctx, string(req.Actor))
	check(t, err)
	recovery := &recoveryEffects{sessionEffects: effects}
	port, err = NewSessions(db, s, recovery, e)
	check(t, err)
	check(t, port.Recover(ctx, req.IntentKey, id))
	after, err := reg.CurrentBinding(ctx, string(req.Actor))
	check(t, err)
	if recovery.recovered != id || after.ID != binding.ID || after.Generation != binding.Generation || after.SessionID != id {
		t.Fatalf("replacement recovery: %s %+v", recovery.recovered, after)
	}
	if err = port.Recover(ctx, req.IntentKey, "different-session"); !errors.Is(err, teams.ErrConflict) {
		t.Fatalf("retarget=%v", err)
	}
	check(t, port.Stop(ctx, req.IntentKey))
	recovery.recovered = ""
	if err = port.Recover(ctx, req.IntentKey, id); !errors.Is(err, teams.ErrDenied) || recovery.recovered != "" {
		t.Fatalf("stopped resurrection: %s %v", recovery.recovered, err)
	}
}

func TestRetainedSessionRecoveryRefusesRevokedBinding(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "state.db"))
	port, e, effects, req := sessionFixture(t, db, s, reg, "revoked")
	id, err := port.Launch(ctx, req)
	check(t, err)
	recovery := &recoveryEffects{sessionEffects: effects}
	port, err = NewSessions(db, s, recovery, e)
	check(t, err)
	check(t, e.ReleaseBinding(ctx, req.IntentKey))
	if err = port.Recover(ctx, req.IntentKey, id); err == nil || recovery.recovered != "" {
		t.Fatalf("revoked recovery: %s %v", recovery.recovered, err)
	}
}
