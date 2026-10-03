package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func recoveryCredential(t *testing.T, svc *Service, id string) (*identity.Store, string) {
	t.Helper()
	ids := identity.NewStore(svc.Store.DB())
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "credential-" + id, Kind: "session", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	return ids, token
}

func TestSessionRecoveryTransitions(t *testing.T) {
	svc := bindingHarness(t)
	runningSession(t, svc, "s", "fake", 123, "2026-10-03T00:00:00Z")
	svc.leaseActorBinding("s", "worker")
	ids, token := recoveryCredential(t, svc, "s")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, unsubscribe, err := svc.Bus.Subscribe(ctx, events.Filter{SessionID: "s", Kinds: []string{events.KindSessionStateChanged}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	var originalStart string
	if err := svc.Store.DB().QueryRow(`SELECT pid_started_at FROM sessions WHERE id=?`, "s").Scan(&originalStart); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkSessionDetached("s", "daemon-shutdown"); err != nil {
		t.Fatal(err)
	}
	row, _ := svc.Store.GetSession("s")
	if row.State != "detached" || row.PID.Int64 != 123 || !row.PID.Valid || row.ExitCode.Valid || row.EndedAt.Valid {
		t.Fatalf("detached row = %+v", row)
	}
	if _, err := ids.Verify(context.Background(), token); err != nil {
		t.Fatalf("detached credential: %v", err)
	}
	if b, err := currentWorkerBinding(svc); err != nil || b.SessionID != "s" {
		t.Fatalf("detached binding: %+v %v", b, err)
	}
	var detachedStart string
	if err := svc.Store.DB().QueryRow(`SELECT pid_started_at FROM sessions WHERE id=?`, "s").Scan(&detachedStart); err != nil || detachedStart != originalStart {
		t.Fatalf("detached process start = %s, %v", detachedStart, err)
	}
	select {
	case live := <-stream:
		if live.Seq == 0 || live.LogicalAgentID != "worker" {
			t.Fatalf("live event = %+v", live)
		}
	case <-ctx.Done():
		t.Fatal("detached transition did not reach live subscribers")
	}
	ev := terminalEvent(t, svc, "s")
	if ev.From != "running" || ev.To != "detached" || ev.Reason != "daemon-shutdown" || ev.ExitCode != nil {
		t.Fatalf("detached event: %+v", ev)
	}
	if err := svc.MarkSessionOrphaned("s", "shim_gone"); err != nil {
		t.Fatal(err)
	}
	row, _ = svc.Store.GetSession("s")
	if row.State != "orphaned" || row.PID.Valid || row.ExitCode.Valid || row.EndedAt.Valid {
		t.Fatalf("orphaned row = %+v", row)
	}
	if _, err := ids.Verify(context.Background(), token); err == nil {
		t.Fatal("orphaned credential remains valid")
	}
	var principalStamp, bindingStamp string
	if err := svc.Store.DB().QueryRow(`SELECT revoked_at FROM principals WHERE session_id=?`, "s").Scan(&principalStamp); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.DB().QueryRow(`SELECT revoked_at FROM runtime_bindings WHERE session_id=?`, "s").Scan(&bindingStamp); err != nil {
		t.Fatal(err)
	}
	if principalStamp != bindingStamp {
		t.Fatalf("revocation timestamps disagree: %s / %s", principalStamp, bindingStamp)
	}
	for name, stamp := range map[string]string{"principal": principalStamp, "binding": bindingStamp} {
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", stamp); err != nil {
			t.Fatalf("%s revocation timestamp: %s", name, stamp)
		}
	}
	if _, err := currentWorkerBinding(svc); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("orphaned binding: %v", err)
	}
	ev = terminalEvent(t, svc, "s")
	if ev.From != "detached" || ev.To != "orphaned" || ev.Reason != "shim_gone" || ev.ExitCode != nil {
		t.Fatalf("orphaned event: %+v", ev)
	}
}

func TestRecoveryRevokesAllBindingGenerations(t *testing.T) {
	svc := bindingHarness(t)
	runningSession(t, svc, "old", "fake", 111, "")
	runningSession(t, svc, "new", "fake", 222, "")
	svc.leaseActorBinding("old", "worker")
	svc.leaseActorBinding("old", "worker")
	svc.leaseActorBinding("new", "worker")
	if err := svc.MarkSessionOrphaned("old", "shim_unreachable"); err != nil {
		t.Fatal(err)
	}
	if b, err := currentWorkerBinding(svc); err != nil || b.SessionID != "new" {
		t.Fatalf("current: %+v %v", b, err)
	}
	if err := svc.MarkSessionOrphaned("new", "shim_gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := currentWorkerBinding(svc); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("superseded orphan binding resurrected: %v", err)
	}
}

func TestReconcileDetachedWithoutShimReconciler(t *testing.T) {
	svc := bindingHarness(t)
	const started = "2026-10-03T00:00:00Z"
	svc.procs = fakeProcesses{123: {started: started, cmd: "fake"}}
	runningSession(t, svc, "detached", "fake", 123, started)
	svc.leaseActorBinding("detached", "worker")
	ids, token := recoveryCredential(t, svc, "detached")
	if err := svc.MarkSessionDetached("detached", "daemon-shutdown"); err != nil {
		t.Fatal(err)
	}
	runningSession(t, svc, "orphan", "fake", 321, "")
	if err := svc.MarkSessionOrphaned("orphan", "shim_gone"); err != nil {
		t.Fatal(err)
	}
	before, _ := svc.Store.GetSession("orphan")
	svc.ReconcileStaleState()
	row, _ := svc.Store.GetSession("detached")
	if row.State != "orphaned" || row.ExitCode.Valid || row.EndedAt.Valid {
		t.Fatalf("swept detached = %+v", row)
	}
	if _, err := ids.Verify(context.Background(), token); err == nil {
		t.Fatal("sweep retained credential")
	}
	if _, err := currentWorkerBinding(svc); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("sweep retained binding: %v", err)
	}
	ev := terminalEvent(t, svc, "detached")
	if ev.From != "detached" || ev.To != "orphaned" || ev.Reason != "shim_reconcile_disabled" {
		t.Fatalf("sweep event: %+v", ev)
	}
	after, _ := svc.Store.GetSession("orphan")
	if *before != *after {
		t.Fatal("sweep changed orphaned session")
	}
}

func TestSweepCanExplicitlySpareDetached(t *testing.T) {
	svc := bindingHarness(t)
	runningSession(t, svc, "s", "fake", 123, "")
	if err := svc.MarkSessionDetached("s", "daemon-shutdown"); err != nil {
		t.Fatal(err)
	}
	swept, spared, err := svc.Store.SweepStaleSessions(time.Now().UTC().Format(time.RFC3339), func(row store.StaleSession) bool { return row.State == "detached" })
	if err != nil || len(swept) != 0 || len(spared) != 1 || spared[0] != "s" {
		t.Fatalf("sweep = %v, %v, %v", swept, spared, err)
	}
	row, _ := svc.Store.GetSession("s")
	if row.State != "detached" || row.PID.Int64 != 123 {
		t.Fatalf("spared row = %+v", row)
	}
}

func TestRecoveryDoesNotReopenTerminalSession(t *testing.T) {
	svc := bindingHarness(t)
	runningSession(t, svc, "s", "fake", 123, "")
	code := 0
	if err := svc.Store.UpdateSessionState("s", "completed", 123, &code); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkSessionDetached("s", "daemon-shutdown"); err == nil {
		t.Fatal("reopened terminal session")
	}
	if err := svc.MarkSessionOrphaned("s", "shim_gone"); err == nil {
		t.Fatal("reopened terminal session")
	}
	if err := svc.MarkSessionDetached("absent", "daemon-shutdown"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := svc.MarkSessionDetached("s", ""); err == nil {
		t.Fatal("accepted missing reason")
	}
	if session.StateDetached.Terminal() || session.StateOrphaned.Terminal() {
		t.Fatal("recovery state is terminal")
	}
}

func TestOrphanTransitionRollsBackWhenRevocationFails(t *testing.T) {
	svc := bindingHarness(t)
	runningSession(t, svc, "s", "fake", 123, "")
	svc.leaseActorBinding("s", "worker")
	ids, token := recoveryCredential(t, svc, "s")
	if _, err := svc.Store.DB().Exec(`CREATE TRIGGER reject_binding_revocation BEFORE UPDATE OF revoked_at ON runtime_bindings BEGIN SELECT RAISE(ABORT, 'simulated revocation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkSessionOrphaned("s", "shim_gone"); err == nil {
		t.Fatal("ignored failed revocation")
	}
	row, _ := svc.Store.GetSession("s")
	if row.State != "running" || row.PID.Int64 != 123 {
		t.Fatalf("partial state change: %+v", row)
	}
	if _, err := ids.Verify(context.Background(), token); err != nil {
		t.Fatalf("partial credential revocation: %v", err)
	}
	if _, err := currentWorkerBinding(svc); err != nil {
		t.Fatalf("partial binding revocation: %v", err)
	}
	evs, err := svc.Store.ListEventsBySession("s", 100, 0)
	if err != nil || len(evs) != 0 {
		t.Fatalf("event emitted for failed change: %v %v", evs, err)
	}
}

func TestRecoverySourceRestrictions(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{"created", "detached"}, {"created", "orphaned"},
		{"ready", "detached"}, {"ready", "orphaned"},
		{"orphaned", "detached"},
	} {
		t.Run(tc.from+"_to_"+tc.to, func(t *testing.T) {
			svc := bindingHarness(t)
			if err := svc.Store.CreateSession(store.SessionRow{ID: "s", State: tc.from}, nil); err != nil {
				t.Fatal(err)
			}
			var err error
			if tc.to == "detached" {
				err = svc.MarkSessionDetached("s", "daemon-shutdown")
			} else {
				err = svc.MarkSessionOrphaned("s", "shim_gone")
			}
			if err == nil {
				t.Fatalf("accepted %s -> %s", tc.from, tc.to)
			}
			row, _ := svc.Store.GetSession("s")
			if row.State != tc.from {
				t.Fatalf("refusal changed state to %s", row.State)
			}
		})
	}
}

func TestRecoveryAllowedSourcesAndIdempotence(t *testing.T) {
	for _, from := range []string{"launching", "running", "detached", "orphaned"} {
		for _, to := range []string{"detached", "orphaned"} {
			if from == "orphaned" && to == "detached" {
				continue
			}
			t.Run(from+"_to_"+to, func(t *testing.T) {
				svc := bindingHarness(t)
				if err := svc.Store.CreateSession(store.SessionRow{ID: "s", State: from}, nil); err != nil {
					t.Fatal(err)
				}
				var err error
				if to == "detached" {
					err = svc.MarkSessionDetached("s", "daemon-shutdown")
				} else {
					err = svc.MarkSessionOrphaned("s", "shim_gone")
				}
				if err != nil {
					t.Fatalf("refused %s -> %s: %v", from, to, err)
				}
				row, _ := svc.Store.GetSession("s")
				if row.State != to {
					t.Fatalf("state = %s", row.State)
				}
				if _, err := time.Parse(time.RFC3339, row.UpdatedAt); err != nil || len(row.UpdatedAt) != len("2006-01-02T15:04:05Z") {
					t.Fatalf("updated_at = %s", row.UpdatedAt)
				}
			})
		}
	}
}
