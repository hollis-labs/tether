package sessionstore_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// wsRace runs fn on n goroutines at once and returns their results.
func wsRace[T any](n int, fn func() (T, error)) ([]T, []error) {
	out, errs := make([]T, n), make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i], errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return out, errs
}

// wsWarm opens n pooled connections first, so racers do not stagger on connection setup (one would
// otherwise finish before the next has a connection, and nothing would overlap).
func wsWarm(t *testing.T, s *store.Stores, n int) {
	t.Helper()
	wsRace(n, func() (struct{}, error) {
		return struct{}{}, s.C().WriteTx(context.Background(), func(context.Context, store.Tx) error { time.Sleep(50 * time.Millisecond); return nil })
	})
}

// wsSuccesses counts nil errors and fails on any error other than allowed.
func wsSuccesses(t *testing.T, errs []error, allowed error) int {
	t.Helper()
	n := 0
	for _, err := range errs {
		switch {
		case err == nil:
			n++
		case !errors.Is(err, allowed):
			t.Fatalf("racer: %v", err)
		}
	}
	return n
}

const wsPlan = `{ "provider": "claude",  "native_state_root": "/old", "z": [1, 2] }`

func wsCreated(t *testing.T, ss *sessionstore.Store, id, plan string) sessionstore.WorkspaceLaunchDecision {
	t.Helper()
	ctx := context.Background()
	r := row(id)
	r.State = "created"
	if _, err := ss.CreateSession(ctx, r, plan, nil); err != nil {
		t.Fatal(err)
	}
	d, err := ss.WorkspaceLaunchDecision(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const (
	wsOp1 = "6f1c1a4e-8a52-4a8e-9a3e-0c5f2b6f2a11"
	wsOp2 = "0b7f4d12-3c4e-4d21-8f0a-5e6d7c8b9a10"
)

func TestWorkspacesLifecycle(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		d := wsCreated(t, ss, "ses-w", wsPlan)
		if d.PlanJSON != wsPlan || d.Session.State != "created" || d.PIDStartedAt != "" {
			t.Fatalf("decision %+v", d)
		}

		// Preparation stamps ref_attribution and the plan's native_state_root, in place and minified
		// (SQLite's json_set).
		clock.Advance(time.Second)
		prep, err := ss.RecordWorkspacePreparation(ctx, d, &sessionstore.WorkspacePreparationStamp{RefAttribution: "ref-1", NativeStateRoot: "/state/a\"b"})
		if err != nil {
			t.Fatal(err)
		}
		wantPlan := `{"provider":"claude","native_state_root":"/state/a\"b","z":[1,2]}`
		if prep.Decision.PlanJSON != wantPlan || prep.Decision.Session.RefAttribution != "ref-1" {
			t.Fatalf("prepared %q %+v", prep.Decision.PlanJSON, prep.Decision.Session)
		}
		if s.C().Dialect().Name() == gostore.DriverSQLite {
			// The Go json_set equals SQLite's own, including an appended key and escapes.
			r := s.C().Reader()
			for i, c := range []struct{ plan, value string }{
				{wsPlan, "/state/a\"b"},
				{`{"a":1}`, "/x\n\t\x01 é"},
				{`{}`, "/y"},
				{`{"native_state_root":{"deep":true},"b":"é"}`, "/z"},
			} {
				var sqlite string
				if err := r.QueryRowContext(ctx, `SELECT json_set(?, '$.native_state_root', ?)`, c.plan, c.value).Scan(&sqlite); err != nil {
					t.Fatal(err)
				}
				d2 := wsCreated(t, ss, fmt.Sprintf("ses-js-%d", i), c.plan)
				got, err := ss.RecordWorkspacePreparation(ctx, d2, &sessionstore.WorkspacePreparationStamp{RefAttribution: "r", NativeStateRoot: c.value})
				if err != nil {
					t.Fatalf("%q: %v", c.plan, err)
				}
				if got.Decision.PlanJSON != sqlite {
					t.Fatalf("json_set of %q: Go %q, SQLite %q", c.plan, got.Decision.PlanJSON, sqlite)
				}
			}
		}

		// A changed decision cannot be prepared again from the stale snapshot.
		if _, err := ss.RecordWorkspacePreparation(ctx, d, nil); !errors.Is(err, sessionstore.ErrWorkspaceAllocationConflict) {
			t.Fatalf("stale preparation: %v", err)
		}

		d = prep.Decision
		res, existed, err := ss.ReservePreparedWorkspaceScratch(ctx, d, wsOp1)
		if err != nil || existed || res.Status != "reserved" || res.Root != "/w/.tether-scratch-"+wsOp1 {
			t.Fatalf("reserve %+v %v %v", res, existed, err)
		}
		if again, existed, err := ss.ReservePreparedWorkspaceScratch(ctx, d, wsOp1); err != nil || !existed || again != res {
			t.Fatalf("repeat reserve %+v %v %v", again, existed, err)
		}
		if _, _, err := ss.ReservePreparedWorkspaceScratch(ctx, d, wsOp2); !errors.Is(err, sessionstore.ErrWorkspaceAllocationConflict) {
			t.Fatalf("second operation: %v", err)
		}
		if _, _, err := ss.ReservePreparedWorkspaceScratch(ctx, d, "not-a-uuid"); !errors.Is(err, sessionstore.ErrWorkspaceAllocationConflict) {
			t.Fatalf("bad operation: %v", err)
		}
		identity := sessionstore.ScratchPhysicalIdentity{Workspace: "dev:1/ino:2", Scratch: "dev:1/ino:3"}
		boom := errors.New("custody lost")
		if _, err := ss.AdmitWorkspaceScratch(ctx, res, identity, func(context.Context) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("failed validate: %v", err)
		}
		if kept, _ := ss.WorkspaceScratchAllocation(ctx, "ses-w"); kept.Status != "reserved" {
			t.Fatalf("a failed admission changed the reservation: %+v", kept)
		}
		admitted, err := ss.AdmitWorkspaceScratch(ctx, res, identity, func(context.Context) error { return nil })
		if err != nil || admitted.Status != "admitted" || admitted.PhysicalIdentity != identity {
			t.Fatalf("admit %+v %v", admitted, err)
		}
		if stored, _ := ss.WorkspaceScratchAllocation(ctx, "ses-w"); stored != admitted {
			t.Fatalf("stored %+v", stored)
		}
		// An earned retry of the preparation keeps the original snapshot.
		if retry, err := ss.RecordWorkspacePreparation(ctx, d, &sessionstore.WorkspacePreparationStamp{RefAttribution: "ref-1", NativeStateRoot: "/state/a\"b"}); err != nil || retry.Decision.PlanJSON != wantPlan {
			t.Fatalf("retry %+v %v", retry, err)
		}
		if _, err := ss.RecordWorkspacePreparation(ctx, d, &sessionstore.WorkspacePreparationStamp{RefAttribution: "ref-2", NativeStateRoot: "/state/a\"b"}); !errors.Is(err, sessionstore.ErrWorkspaceAllocationConflict) {
			t.Fatalf("retry with another stamp: %v", err)
		}

		clock.Advance(time.Second)
		launching, err := ss.BeginWorkspaceScratchPlacement(ctx, d, admitted)
		if err != nil || launching.Session.State != "launching" || launching.Session.PID == nil || *launching.Session.PID != 0 {
			t.Fatalf("begin %+v %v", launching.Session, err)
		}
		if err := ss.ValidateWorkspaceScratchPlacement(ctx, launching, admitted); err != nil {
			t.Fatal(err)
		}
		if err := ss.ValidateWorkspaceScratchStart(ctx, d, admitted); err != nil {
			t.Fatal(err)
		}
		if _, err := ss.BeginWorkspaceScratchPlacement(ctx, d, admitted); !errors.Is(err, sessionstore.ErrWorkspaceAllocationConflict) {
			t.Fatalf("second begin: %v", err)
		}
		if err := ss.UpdateSessionState(ctx, "ses-w", "running", 42, nil); err != nil {
			t.Fatal(err)
		}
		if err := ss.ValidateWorkspaceScratchStart(ctx, d, admitted); !errors.Is(err, sessionstore.ErrWorkspaceAllocationConflict) {
			t.Fatalf("start after the process ran: %v", err)
		}
		if err := ss.ValidateWorkspaceScratchPlacement(ctx, launching, admitted); !errors.Is(err, sessionstore.ErrWorkspaceAllocationConflict) {
			t.Fatalf("placement after the process ran: %v", err)
		}
	})
}

// TestWorkspacesConcurrent races each check-then-act path: one reservation of a session (the others
// conflict), one preparation stamp and one created -> launching transition. On Postgres the racers
// overlap and only the decision lock keeps them to one.
func TestWorkspacesConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		const n = 6
		ops := []string{wsOp1, wsOp2, "1a2b3c4d-0000-4000-8000-000000000001", "1a2b3c4d-0000-4000-8000-000000000002",
			"1a2b3c4d-0000-4000-8000-000000000003", "1a2b3c4d-0000-4000-8000-000000000004"}

		d := wsCreated(t, ss, "ses-prep", wsPlan)
		wsWarm(t, s, n)
		_, errs := wsRace(n, func() (sessionstore.WorkspacePreparationResult, error) {
			return ss.RecordWorkspacePreparation(ctx, d, &sessionstore.WorkspacePreparationStamp{RefAttribution: "ref", NativeStateRoot: "/state"})
		})
		if got := wsSuccesses(t, errs, sessionstore.ErrWorkspaceAllocationConflict); got != 1 {
			t.Fatalf("%d preparations recorded, want 1", got)
		}

		d, _ = ss.WorkspaceLaunchDecision(ctx, "ses-prep")
		var mu sync.Mutex
		next := 0
		wsWarm(t, s, n)
		_, errs = wsRace(n, func() (sessionstore.WorkspaceScratchAllocation, error) {
			mu.Lock()
			op := ops[next]
			next++
			mu.Unlock()
			a, _, err := ss.ReservePreparedWorkspaceScratch(ctx, d, op)
			return a, err
		})
		if got := wsSuccesses(t, errs, sessionstore.ErrWorkspaceAllocationConflict); got != 1 {
			t.Fatalf("%d reservations, want 1", got)
		}
		res, err := ss.WorkspaceScratchAllocation(ctx, "ses-prep")
		if err != nil {
			t.Fatal(err)
		}
		admitted, err := ss.AdmitWorkspaceScratch(ctx, res, sessionstore.ScratchPhysicalIdentity{Workspace: "w", Scratch: "s"}, func(context.Context) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		wsWarm(t, s, n)
		_, errs = wsRace(n, func() (sessionstore.WorkspaceLaunchDecision, error) {
			return ss.BeginWorkspaceScratchPlacement(ctx, d, admitted)
		})
		if got := wsSuccesses(t, errs, sessionstore.ErrWorkspaceAllocationConflict); got != 1 {
			t.Fatalf("%d placements began, want 1", got)
		}
	})
}

func TestCheckpoints(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		for _, c := range []sessionstore.CheckpointRow{
			{ID: "cp-1", LogicalAgentID: "agent", CreatedAt: "2026-11-14T10:00:00Z", Summary: "first", ProviderHintsJSON: `{"k":1}`},
			{ID: "cp-3", LogicalAgentID: "agent", CreatedAt: "2026-11-14T11:00:00Z", TaskID: "T-1"},
			{ID: "cp-2", LogicalAgentID: "agent", CreatedAt: "2026-11-14T11:00:00Z"},
			{ID: "cp-x", LogicalAgentID: "other", CreatedAt: "2026-11-14T12:00:00Z"},
		} {
			if err := ss.CreateCheckpoint(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		if err := ss.CreateCheckpoint(ctx, sessionstore.CheckpointRow{ID: "cp-bad", LogicalAgentID: "agent"}); err == nil {
			t.Fatal("a checkpoint without created_at was accepted")
		}
		got, err := ss.GetCheckpoint(ctx, "cp-1")
		if err != nil || got.Summary != "first" || got.ProviderHintsJSON != `{"k":1}` || got.TaskID != "" {
			t.Fatalf("get %+v %v", got, err)
		}
		if _, err := ss.GetCheckpoint(ctx, "nope"); !errors.Is(err, sessionstore.ErrCheckpointNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if latest, _ := ss.LatestCheckpointForAgent(ctx, "agent"); latest.ID != "cp-3" || latest.TaskID != "T-1" {
			t.Fatalf("latest %+v", latest)
		}
		if _, err := ss.LatestCheckpointForAgent(ctx, "nobody"); !errors.Is(err, sessionstore.ErrCheckpointNotFound) {
			t.Fatalf("latest for nobody: %v", err)
		}
		list, _ := ss.ListCheckpointsByLogicalAgent(ctx, "agent")
		if len(list) != 3 || list[0].ID != "cp-3" || list[1].ID != "cp-2" || list[2].ID != "cp-1" {
			t.Fatalf("list %+v", list)
		}

		for _, a := range [][3]string{{"att-2", "cli", "2026-11-14T10:00:00Z"}, {"att-1", "web", "2026-11-14T10:00:00Z"}, {"att-3", "cli", "2026-11-14T09:00:00Z"}} {
			if err := ss.CreateClientAttachment(ctx, a[0], "ses-1", a[1], a[2]); err != nil {
				t.Fatal(err)
			}
		}
		if err := ss.DetachClientAttachment(ctx, "att-3", "2026-11-14T09:30:00Z"); err != nil {
			t.Fatal(err)
		}
		atts, _ := ss.ListClientAttachments(ctx, "ses-1")
		if len(atts) != 3 || atts[0].ID != "att-3" || atts[0].DetachedAt == "" || atts[1].ID != "att-1" || atts[2].ID != "att-2" || atts[1].ClientKind != "web" {
			t.Fatalf("attachments %+v", atts)
		}
		if n, err := ss.SweepStaleAttachments(ctx, "2026-11-14T13:00:00Z"); err != nil || n != 2 {
			t.Fatalf("sweep %d %v", n, err)
		}
	})
}

func TestSeats(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		const urn = "msg://agent/cell/bob"
		if _, found, err := ss.GetSeatStatus(ctx, urn); err != nil || found {
			t.Fatalf("before any: %v %v", found, err)
		}
		t1 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		d := sessionstore.SeatStatusRow{AgentURN: urn, State: "working", Reason: "r", Ref: "task:T-1", SetBy: urn, Since: t1, UpdatedAt: t1}
		if _, changed, err := ss.PutSeatStatus(ctx, d); err != nil || !changed {
			t.Fatalf("first put %v %v", changed, err)
		}
		again := d
		again.Since, again.UpdatedAt, again.ExpiresAt = t1.Add(time.Hour), t1.Add(time.Hour), t1.Add(2*time.Hour)
		stored, changed, err := ss.PutSeatStatus(ctx, again)
		if err != nil || changed || !stored.Since.Equal(t1) {
			t.Fatalf("unchanged put %+v %v %v", stored, changed, err)
		}
		got, found, _ := ss.GetSeatStatus(ctx, urn)
		if !found || !got.Since.Equal(t1) || !got.UpdatedAt.Equal(t1.Add(time.Hour)) || !got.ExpiresAt.Equal(t1.Add(2*time.Hour)) || got.State != "working" {
			t.Fatalf("stored %+v", got)
		}
		blocked := again
		blocked.State, blocked.Owner, blocked.ExpiresAt = "blocked", "@lead", time.Time{}
		if _, changed, err := ss.PutSeatStatus(ctx, blocked); err != nil || !changed {
			t.Fatalf("state change %v %v", changed, err)
		}
		if got, _, _ = ss.GetSeatStatus(ctx, urn); !got.ExpiresAt.IsZero() || got.Owner != "@lead" {
			t.Fatalf("after change %+v", got)
		}

		// Concurrent identical first declarations: exactly one reports changed.
		const n = 6
		fresh := d
		fresh.AgentURN = "msg://agent/cell/carol"
		wsWarm(t, s, n)
		changes, errs := wsRace(n, func() (bool, error) {
			_, changed, err := ss.PutSeatStatus(ctx, fresh)
			return changed, err
		})
		count := 0
		for i, c := range changes {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if c {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%d concurrent puts reported changed, want 1", count)
		}
	})
}
