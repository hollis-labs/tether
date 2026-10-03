package teamhost_test

import (
	"errors"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

func TestExplicitTrustDurableApprovalAndManualSignal(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/state.db", f)
	run, root := launch(t, s, h)
	target := definition().Slots[1]
	makeHost := func(opts teamhost.Options) *teamhost.Host {
		host, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: f, Enroller: f, Messenger: f, Channels: f}, opts)
		must(t, err)
		return host
	}
	denied := makeHost(teamhost.Options{})
	decision, err := denied.ResolveTrust(ctx, root, target)
	must(t, err)
	if decision != teams.TrustDeny {
		t.Fatal("trust allowed by omission")
	}
	opts := options()
	opts.TrustTiers[target.Definition] = teamhost.TrustApproval
	approval := makeHost(opts)
	decision, err = approval.ResolveTrust(ctx, root, target)
	must(t, err)
	if decision != teams.TrustApproval {
		t.Fatal("approval tier bypassed")
	}
	if _, err := teams.Spawn(ctx, definition(), s, approval, approval, approval, run.ID, root.Actor, "worker", "approval", childLimits(), limits()); !errors.Is(err, teams.ErrDenied) {
		t.Fatalf("approval not required: %v", err)
	}
	var key string
	must(t, db.DB().QueryRow(`SELECT request_key FROM team_host_approvals`).Scan(&key))
	must(t, approval.EmitApproval(ctx, key, root, target))
	changed := target
	changed.Role = "changed"
	if err := approval.EmitApproval(ctx, key, root, changed); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("approval rebound")
	}
	opts = options()
	opts.ActorTrust = map[mesh.URN]teamhost.TrustTier{"msg://user/test/operator": teamhost.TrustTrusted}
	principal := teams.Member{Actor: "msg://user/test/operator", Governance: teams.Owner}
	trusted := makeHost(opts)
	decision, err = trusted.ResolveTrust(ctx, principal, target)
	must(t, err)
	if decision != teams.TrustAllow {
		t.Fatal("explicit principal tier not honored")
	}
	decision, err = trusted.ResolveTrust(ctx, teams.Member{Actor: "msg://user/test/other"}, target)
	must(t, err)
	if decision != teams.TrustDeny {
		t.Fatal("unclassified caller allowed")
	}
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	_, err = teams.Signal(ctx, definition(), roster, definition().Phases[0], root.Actor, s, h)
	must(t, err)
	_, err = teams.Advance(ctx, definition(), roster, definition().Phases[0], root.Actor, s)
	must(t, err)
	if _, err := h.Evaluate(ctx, teams.Trigger{Kind: "event"}, root); !errors.Is(err, teams.ErrUnsupported) {
		t.Fatal("unsupported trigger swallowed")
	}
	if _, err := h.Evaluate(ctx, teams.Trigger{Kind: "manual"}, teams.Member{}); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal("inactive manual signal fired")
	}
	must(t, h.InstallRouting(ctx, "launch", run, definition().Routing))
	if err := h.InstallRouting(ctx, "other", run, definition().Routing); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("routing key rebound")
	}
	must(t, h.RemoveRouting(ctx, run.ID))
	must(t, h.RemoveRouting(ctx, run.ID))
	if err := h.InstallRouting(ctx, "launch", run, definition().Routing); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("routing resurrected after removal")
	}
}
