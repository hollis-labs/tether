package teamruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

type bootContextEffects struct {
	*sessionEffects
	accepted []app.TeamBootContext
}

func (f *bootContextEffects) CreateTeamSessionWithContext(ctx context.Context, key, launch string, boot app.TeamBootContext) (*app.Launched, error) {
	f.accepted = append(f.accepted, boot)
	return f.CreateTeamSession(ctx, key, launch)
}

func TestTeamBootContextPortKeepsAcceptedIntentAndRefusesUnsupportedPort(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "accepted"}[supported], func(t *testing.T) {
			db, storage, reg := dbFixture(t, filepath.Join(t.TempDir(), "state.db"))
			enrollment := enroller(t, db, storage, reg)
			intent := fresh("context")
			intent.Provision.RunID = "run"
			intent.Provision.Slot.Workspace = map[string]string{"mission": "accepted mission", "brief": "accepted brief", "unrelated": "not interpreted"}
			actor, err := enrollment.Ensure(ctx, intent)
			check(t, err)
			check(t, enrollment.AcquireBinding(ctx, "context", actor.Actor))
			effects := &sessionEffects{db: db}
			port, err := NewSessions(db, storage, effects, enrollment)
			check(t, err)
			req := teamhost.SessionRequest{IntentKey: "context", Actor: actor.Actor, Provision: intent.Provision}
			contextual := &bootContextEffects{sessionEffects: effects}
			if supported {
				port.service = contextual
			}
			id, err := port.Launch(ctx, req)
			if !supported {
				if !errors.Is(err, teamhost.ErrSessionUnavailable) || id != "" || effects.launched != 0 {
					t.Fatal(id, err, effects.launched)
				}
				rows, listErr := db.ListSessions(store.ListSessionsOptions{Limit: 10})
				check(t, listErr)
				if len(rows) != 0 {
					t.Fatal("unsupported context created session", rows)
				}
				return
			}
			check(t, err)
			if len(contextual.accepted) != 1 || contextual.accepted[0].Mission != "accepted mission" || contextual.accepted[0].Brief != "accepted brief" || contextual.accepted[0].RunID != "run" || contextual.accepted[0].Slot != req.Provision.Slot.Name {
				t.Fatal(contextual.accepted)
			}
			replay, err := port.Launch(ctx, req)
			check(t, err)
			if replay != id || effects.launched != 1 {
				t.Fatal("retry relaunched", replay, effects.launched)
			}
			req.Provision.Slot.Workspace["brief"] = "changed"
			if _, err = port.Launch(ctx, req); !errors.Is(err, teams.ErrConflict) {
				t.Fatal("changed intent accepted", err)
			}
		})
	}
}
