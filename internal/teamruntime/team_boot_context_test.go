package teamruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

type bootContextEffects struct {
	*sessionEffects
	accepted []app.TeamBootContext
}

func TestInitialFormationWithMissionAndBriefUsesActualRunID(t *testing.T) {
	f, _ := newPolicyFixture(t, stageAPolicy)
	effects := &bootContextEffects{sessionEffects: f.effects}
	f.sessions.service = effects
	team := stageATeam()
	for i := range team.Slots {
		if team.Slots[i].Min > 0 {
			team.Slots[i].Workspace = map[string]string{"mission": "Report ready", "brief": "End your turn"}
		}
	}
	op := operatorContext(t)
	request := teamsvc.FormRequest{Key: "boot-with-context", Team: team}
	result, err := f.svc.Form(op, request)
	check(t, err)
	if result.Run == nil || result.Run.ID == "" || len(effects.accepted) != 2 {
		t.Fatalf("formation did not provision both members: %+v %+v", result, effects.accepted)
	}
	for _, boot := range effects.accepted {
		if boot.RunID != result.Run.ID || boot.Mission != "Report ready" || boot.Brief != "End your turn" || boot.Slot == "" {
			t.Fatalf("boot context differs from committed run: %+v", boot)
		}
	}
	// Retrying the accepted request reuses the exact run and member sessions.
	replay, err := f.svc.Form(op, request)
	check(t, err)
	if replay.Run == nil || replay.Run.ID != result.Run.ID || f.effects.launched != 2 {
		t.Fatalf("formation replay changed sessions: %+v launches=%d", replay, f.effects.launched)
	}
	// The workaround enriches only the boot view, not the retained v0.6 intent.
	var changed int
	check(t, f.effects.db.DB().QueryRow(`SELECT count(*) FROM team_host_intents WHERE json_extract(request,'$.RunID')<>''`).Scan(&changed))
	if changed != 0 {
		t.Fatal("initial provision requests were rewritten")
	}
}

func (f *bootContextEffects) CreateTeamSessionWithContext(ctx context.Context, key, launch string, boot app.TeamBootContext) (*app.Launched, error) {
	f.accepted = append(f.accepted, boot)
	return f.CreateTeamSession(ctx, key, launch)
}

func (f *bootContextEffects) CreateTeamSessionWithLimits(ctx context.Context, key, launch string, boot app.TeamBootContext, _ time.Duration) (*app.Launched, error) {
	return f.CreateTeamSessionWithContext(ctx, key, launch, boot)
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

func TestLaunchEnforcesSpawnTimeoutAtCreate(t *testing.T) {
	db, storage, reg := dbFixture(t, filepath.Join(t.TempDir(), "state.db"))
	enrollment := enroller(t, db, storage, reg)
	intent := fresh("timeout")
	intent.Provision.Limits.Timeout = 90 * time.Second
	actor, err := enrollment.Ensure(ctx, intent)
	check(t, err)
	check(t, enrollment.AcquireBinding(ctx, "timeout", actor.Actor))
	effects := &sessionEffects{db: db}
	port, err := NewSessions(db, storage, effects, enrollment)
	check(t, err)
	_, err = port.Launch(ctx, teamhost.SessionRequest{IntentKey: "timeout", Actor: actor.Actor, Provision: intent.Provision})
	check(t, err)
	if len(effects.timeouts) != 1 || effects.timeouts[0] != 90*time.Second {
		t.Fatal(effects.timeouts)
	}
}
