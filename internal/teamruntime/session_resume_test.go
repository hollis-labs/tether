package teamruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/mesh/teams"
)

type recoveryEffects struct {
	*sessionEffects
	recovered string
}

func (e *recoveryEffects) RecoverTeamSession(_ context.Context, _ string, id string) error {
	e.recovered = id
	return nil
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
