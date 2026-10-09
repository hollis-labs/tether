package teamhost_test

import (
	"context"
	"testing"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

type recoveryPorts struct {
	*fakePorts
	recovered map[string]string
}

func (p *recoveryPorts) Recover(_ context.Context, key, id string) error {
	p.recovered[key] = id
	return nil
}

func TestRetainedRecoveryIncludesOriginalFreshAndPreservesStopped(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/state.db", f)
	run, root := launch(t, s, h)
	child := spawn(t, s, h, run, root, "child")
	must(t, teams.CancelMembers(ctx, definition(), s, h, run.ID, root.Actor, child.ID, true))
	p := &recoveryPorts{fakePorts: f, recovered: map[string]string{}}
	h, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: p, Enroller: f, Messenger: f, Channels: f}, options())
	must(t, err)
	must(t, h.RecoverSessions(ctx))
	if len(p.recovered) != 1 {
		t.Fatalf("recovered=%v", p.recovered)
	}
	for _, id := range p.recovered {
		if id != root.SessionID {
			t.Fatalf("replaced or revived session: %s", id)
		}
	}
	current, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	for _, m := range current.Members {
		if m.ID == root.ID && (m.Actor != root.Actor || m.SessionID != root.SessionID) {
			t.Fatal("retained identity changed")
		}
	}
}
