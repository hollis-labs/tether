//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/shimhost"
)

// detachedShimFixture starts the shim session and drops only the local bridge,
// leaving a detached session with a canonical placement receipt.
func detachedShimFixture(t *testing.T) (*shimAppFixture, shimhost.Receipt) {
	t.Helper()
	f := shimFixture(t)
	r := f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.svc.Manager.Stop(ctx, f.req.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = f.svc.Manager.WaitSession(ctx, f.req.ID)
	if err := f.svc.waitShimBinding(ctx, f.req.ID); err != nil {
		t.Fatal(err)
	}
	if row, _ := f.svc.Store.GetSession(f.req.ID); row.State != "detached" {
		t.Fatalf("fixture state=%s", row.State)
	}
	return f, r
}

func requireRetireRefused(t *testing.T, f *shimAppFixture, err error, want string) {
	t.Helper()
	if !errors.Is(err, api.ErrShimRetireRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("want refusal containing %q, got %v", want, err)
	}
	if row, _ := f.svc.Store.GetSession(f.req.ID); row.State != "detached" {
		t.Fatalf("refused retire changed state to %s", row.State)
	}
}

func TestRetireShimSessionRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("host still present", func(t *testing.T) {
		f, r := detachedShimFixture(t)
		f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
			return shimhost.Inspection{Receipt: r, Running: true}, nil
		}
		f.svc.shimHosting.stop = func(context.Context, shimhost.Receipt) error {
			t.Error("stopProvider reached for a present host")
			return nil
		}
		requireRetireRefused(t, f, f.svc.RetireShimSession(ctx, f.req.ID, "op", false), "still present")
	})
	t.Run("unreadable receipt", func(t *testing.T) {
		f, r := detachedShimFixture(t)
		if err := os.Remove(r.DescriptorPath[:strings.LastIndex(r.DescriptorPath, "/")] + "/placement.json"); err != nil {
			t.Fatal(err)
		}
		requireRetireRefused(t, f, f.svc.RetireShimSession(ctx, f.req.ID, "op", false), "cannot verify the host is gone")
	})
	t.Run("team member", func(t *testing.T) {
		f, _ := detachedShimFixture(t)
		if _, err := f.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.team_member',json('true')) WHERE session_id=?`, f.req.ID); err != nil {
			t.Fatal(err)
		}
		requireRetireRefused(t, f, f.svc.RetireShimSession(ctx, f.req.ID, "op", true), "team recovery")
	})
	t.Run("codex without acknowledgement", func(t *testing.T) {
		f, _ := detachedShimFixture(t)
		if _, err := f.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.provider_brand','codex') WHERE session_id=?`, f.req.ID); err != nil {
			t.Fatal(err)
		}
		requireRetireRefused(t, f, f.svc.RetireShimSession(ctx, f.req.ID, "op", false), "acknowledge_obligations")
	})
}

func TestRetireShimSessionGoneHostOrphansAndAudits(t *testing.T) {
	f, r := detachedShimFixture(t)
	stops := 0
	f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
		return shimhost.Inspection{Receipt: r, Gone: true}, nil
	}
	f.svc.shimHosting.stop = func(context.Context, shimhost.Receipt) error { stops++; return nil }
	sub, cancel, err := f.svc.Bus.Subscribe(context.Background(), events.Filter{Kinds: []string{events.KindSessionShimRetired}})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := f.svc.RetireShimSession(context.Background(), f.req.ID, "op", false); err != nil {
		t.Fatal(err)
	}
	if stops != 1 {
		t.Fatalf("stopProvider calls=%d", stops)
	}
	if row, _ := f.svc.Store.GetSession(f.req.ID); row.State != "orphaned" {
		t.Fatalf("state=%s", row.State)
	}
	select {
	case e := <-sub:
		if e.Kind != events.KindSessionShimRetired {
			t.Fatalf("event kind=%s", e.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session.shim_retired not published")
	}
}
