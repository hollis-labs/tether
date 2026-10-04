//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/shimhost"
)

func TestShimReconcilerClassification(t *testing.T) {
	for _, tc := range []struct {
		code string
		gone bool
		want string
	}{
		{"outcome_unknown", false, "detached"}, {"identity_mismatch", false, "detached"},
		{"journal_mismatch", false, "detached"}, {"unauthorized", false, "detached"},
		{"stale_controller", false, "detached"}, {"timeout", false, "detached"},
		{"", true, "orphaned"},
	} {
		t.Run(tc.code+tc.want, func(t *testing.T) {
			f := shimFixture(t)
			host := f.svc.shimHosting
			intent := host.provider.PlacementIdentity(f.req.ID+":1", shimLaunchIdentity(f))
			if err := shimhost.PrivateDir(filepath.Dir(intent.DescriptorPath)); err != nil {
				t.Fatal(err)
			}
			if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(intent.DescriptorPath), "placement.json"), intent); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.persistShim(context.Background(), f.req.ID, "claude", "boot", intent); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.Store.UpdateSessionState(f.req.ID, "running", 0, nil); err != nil {
				t.Fatal(err)
			}
			f.svc.leaseActorBinding(f.req.ID, "worker")
			ids, token := recoveryCredential(t, f.svc, f.req.ID)
			calls := 0
			host.inspect = func(_ context.Context, r shimhost.Receipt) (shimhost.Inspection, error) {
				calls++
				if r.OperationKey != intent.OperationKey {
					t.Fatal("canonical identity lost")
				}
				if tc.code == "timeout" {
					return shimhost.Inspection{}, context.DeadlineExceeded
				}
				if tc.code != "" {
					return shimhost.Inspection{Gone: true}, &shimhost.Failure{Code: tc.code}
				}
				return shimhost.Inspection{Gone: tc.gone}, nil
			}
			f.svc.ReconcileStaleState()
			row, _ := f.svc.Store.GetSession(f.req.ID)
			if row.State != tc.want || calls != 1 {
				t.Fatalf("state=%s calls=%d", row.State, calls)
			}
			_, credentialErr := ids.Verify(context.Background(), token)
			_, bindingErr := currentWorkerBinding(f.svc)
			if tc.gone {
				if credentialErr == nil || !errors.Is(bindingErr, registry.ErrBindingNotFound) {
					t.Fatal("positive absence retained authority")
				}
			} else {
				if credentialErr != nil || bindingErr != nil {
					t.Fatal("unknown/refusal revoked authority")
				}
				f.svc.ReconcileStaleState()
				if calls != 2 {
					t.Fatal("detached outcome not retried")
				}
			}
		})
	}
}

func TestShimFlagOffRetainsDetachedChildIdentity(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.svc.DrainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvLaunchHost, "direct")
	f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
		t.Fatal("flag off inspected shim")
		return shimhost.Inspection{}, nil
	}
	f.svc.ReconcileStaleState()
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "detached" {
		t.Fatalf("disabled reconciler state=%s", row.State)
	}
	if _, err := os.Stat(r.DescriptorPath); err != nil {
		t.Fatal("disabled reconciler forgot capability")
	}
	result, err := f.svc.shimHosting.provider.Inspect(ctx, r)
	if err != nil || !result.Running {
		t.Fatalf("flag off killed provider: %v", err)
	}
}

func TestShimRebuildServiceReattachesAndRunsNextTurn(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	if err := f.svc.SendTurn(context.Background(), f.req.ID, "before"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "first published turn", func() bool { return len(outputEvents(t, f.svc)) == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.svc.DrainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	mgr, stops := newSessionManager(f.svc.Store, f.svc.Bus)
	restarted := &Service{Catalog: f.svc.Catalog, Store: f.svc.Store, Bus: f.svc.Bus, Registry: f.svc.Registry, Manager: mgr, stops: stops, shimHosting: f.svc.shimHosting, turnFeeds: f.svc.turnFeeds}
	t.Cleanup(func() { _ = restarted.StopSession(f.req.ID); _ = mgr.Shutdown(context.Background()) })
	restarted.ReconcileStaleState()
	row, _ := restarted.Store.GetSession(f.req.ID)
	if row.State != "running" {
		t.Fatalf("reattach state=%s", row.State)
	}
	if err := restarted.SendTurn(context.Background(), f.req.ID, "after"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "next published turn", func() bool { return len(outputEvents(t, restarted)) == 2 })
	shimRow, _ := restarted.Store.SessionShim(context.Background(), f.req.ID)
	canonical, err := loadShimReceipt(shimRow)
	if err != nil || canonical.HostPID != r.HostPID || canonical.ProviderPID != r.ProviderPID {
		t.Fatalf("reattach replaced child: %v", err)
	}
	if err := restarted.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanedSessionCannotMintAnotherPrincipal(t *testing.T) {
	svc := bindingHarness(t)
	runningSession(t, svc, "orphan", "fake", 0, "2026-10-03T00:00:00Z")
	if err := svc.MarkSessionOrphaned("orphan", "shim_gone"); err != nil {
		t.Fatal(err)
	}
	_, err := identity.NewStore(svc.Store.DB()).Mint(context.Background(), identity.Principal{ID: "new-principal", Kind: "session", SessionID: "orphan"})
	if err == nil {
		t.Fatal("orphaned session gained new authority")
	}
	var n int
	if err := svc.Store.DB().QueryRow(`SELECT COUNT(*) FROM principals WHERE session_id='orphan'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("guarded insert persisted credential: %v", err)
	}
}

func shimLaunchIdentity(f *shimAppFixture) shim.Launch {
	return shim.Launch{Session: f.req.ID, Instance: f.svc.shimHosting.instance, Generation: 1}
}
