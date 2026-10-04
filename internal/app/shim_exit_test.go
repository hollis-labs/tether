//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

func TestShimNaturalExitPublishesTerminalState(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	ids, token := recoveryCredential(t, f.svc, f.req.ID)
	if err := f.svc.SendTurn(context.Background(), f.req.ID, "exit"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "provider terminal state", func() bool { row, _ := f.svc.Store.GetSession(f.req.ID); return row.State == "failed" })
	if err := f.svc.waitShimBinding(context.Background(), f.req.ID); err != nil {
		t.Fatal(err)
	}
	event := terminalEvent(t, f.svc, f.req.ID)
	if event.To != "failed" || event.ExitCode == nil || *event.ExitCode != 7 {
		t.Fatalf("terminal event=%+v", event)
	}
	if _, err := os.Stat(r.DescriptorPath); !os.IsNotExist(err) {
		t.Fatal("exited provider retained capability")
	}
	shimAwait(t, "exit revokes authority", func() bool { _, err := ids.Verify(context.Background(), token); return err != nil })
}

func TestShimPrechildPlacementFailuresUseDirectRequest(t *testing.T) {
	for _, code := range []string{"placement_failed", "placement_retired"} {
		t.Run(code, func(t *testing.T) {
			f := shimFixture(t)
			f.svc.shimHosting.place = func(_ context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
				r := f.svc.shimHosting.provider.PlacementIdentity(key, spec)
				r.Attempted = true
				if code == "placement_failed" {
					r.PlacementFailure = code
				} else {
					r.Retired = true
				}
				return r, &shimhost.Failure{Code: code}
			}
			got, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
			if err != nil || got.Runtime != f.req.Runtime {
				t.Fatalf("prechild fallback: %v", err)
			}
			if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
				t.Fatalf("unstarted row retained: %v", err)
			}
		})
	}
}

func TestShimReattachRefusesConflictingCheckpoint(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	if err := f.svc.DrainSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(r.DescriptorPath), "bridge.json")
	cp, err := shimbridge.ReadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	cp.Journal = "conflicting-journal"
	if err := shimhost.WritePrivateJSON(path, cp); err != nil {
		t.Fatal(err)
	}
	row, _ := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err := f.svc.reattachShim(context.Background(), row, r); shimFailureCode(err) != "journal_mismatch" {
		t.Fatalf("attach accepted conflict: %v", err)
	}
	if _, live := f.svc.Manager.Get(f.req.ID); live {
		t.Fatal("refusal started bridge")
	}
	if _, err := os.Stat(r.DescriptorPath); err != nil {
		t.Fatal("refusal deleted capability")
	}
}
