package app

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

// unknownProcs reports every pid alive with an unreadable start time: the reaper must retain such a
// session (identity unknown) and, the first time it does, back off from it.
type unknownProcs struct{}

func (unknownProcs) alive(int) bool               { return true }
func (unknownProcs) startTime(int) (string, bool) { return "", false }
func (unknownProcs) command(int) (string, bool)   { return "", false }

func createRetained(t *testing.T, svc *Service, id string) {
	t.Helper()
	if err := svc.Store.CreateSession(store.SessionRow{ID: id, State: "running", Workspace: t.TempDir()}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET pid=? WHERE id=?`, os.Getpid(), id); err != nil {
		t.Fatal(err)
	}
}

func TestReaperBackoffSchedule(t *testing.T) {
	cases := []struct {
		fails int
		want  time.Duration
	}{{0, 10 * time.Minute}, {1, 10 * time.Minute}, {2, 20 * time.Minute}, {3, 40 * time.Minute}, {6, 5*time.Hour + 20*time.Minute}, {7, reaperBackoffMax}, {50, reaperBackoffMax}}
	for _, c := range cases {
		if got := reaperBackoffFor(c.fails); got != c.want {
			t.Errorf("fails=%d got=%s want=%s", c.fails, got, c.want)
		}
	}
	if sessionReaperInterval != 5*time.Minute {
		t.Fatalf("interval=%s", sessionReaperInterval)
	}
}

// A session the reaper cannot resolve (here: a live process of unknown identity) is examined once, then
// skipped without any lookups until its identity signature changes or the backoff expires.
func TestReaperSkipsUnresolvedSessionUntilBackoffExpires(t *testing.T) {
	svc := stopHarness(t)
	svc.procs = unknownProcs{}
	createRetained(t, svc, "stuck")
	r := svc.reaperState()
	base := time.Now().Add(time.Minute)
	if err := svc.SweepSessionReaper(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if r.lastExamined != 1 || r.lastSkipped != 0 {
		t.Fatalf("first sweep examined=%d skipped=%d", r.lastExamined, r.lastSkipped)
	}
	if got, _ := svc.Store.GetSession("stuck"); got.State != "running" || !got.PID.Valid {
		t.Fatalf("session after first sweep: state=%s pid=%+v", got.State, got.PID)
	}
	if err := svc.SweepSessionReaper(context.Background(), base.Add(sessionReaperInterval)); err != nil {
		t.Fatal(err)
	}
	if r.lastExamined != 0 || r.lastSkipped != 1 {
		t.Fatalf("second sweep (inside backoff) examined=%d skipped=%d", r.lastExamined, r.lastSkipped)
	}
	// Past the first backoff the session is looked at again, and the next backoff doubles.
	if err := svc.SweepSessionReaper(context.Background(), base.Add(reaperBackoffFor(1)+time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.lastExamined != 1 {
		t.Fatalf("after backoff examined=%d", r.lastExamined)
	}
	v, _ := r.backoff.Load("stuck")
	if b := v.(reaperBackoff); b.fails != 2 {
		t.Fatalf("fails=%d want 2", b.fails)
	}
	// A real change of identity (the process id) clears the skip immediately.
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET pid_started_at='x' WHERE id='stuck'`); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepSessionReaper(context.Background(), base.Add(reaperBackoffFor(1)+2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.lastExamined != 1 {
		t.Fatalf("identity change did not re-examine: examined=%d", r.lastExamined)
	}
}

// One sweep never looks at more than reaperMaxPerSweep sessions; the rest follow on the next sweeps, so
// every session is eventually examined and a large backlog cannot monopolise the daemon.
func TestReaperSweepIsBoundedAndRotates(t *testing.T) {
	svc := stopHarness(t)
	svc.procs = unknownProcs{}
	n := reaperMaxPerSweep + 20
	for i := 0; i < n; i++ {
		createRetained(t, svc, fmt.Sprintf("s%03d", i))
	}
	r := svc.reaperState()
	now := time.Now().Add(time.Minute)
	if err := svc.SweepSessionReaper(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if r.lastExamined != reaperMaxPerSweep {
		t.Fatalf("first sweep examined=%d want %d", r.lastExamined, reaperMaxPerSweep)
	}
	if err := svc.SweepSessionReaper(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.lastExamined != 20 || r.lastSkipped != reaperMaxPerSweep {
		t.Fatalf("second sweep examined=%d skipped=%d", r.lastExamined, r.lastSkipped)
	}
	// Everything is now parked: the steady state does no per-session work at all.
	if err := svc.SweepSessionReaper(context.Background(), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.lastExamined != 0 || r.lastSkipped != n {
		t.Fatalf("steady sweep examined=%d skipped=%d", r.lastExamined, r.lastSkipped)
	}
}

// A retained condition that does not change must not rewrite the session or append a shim_status event
// on every reconcile (the old behaviour produced an unbounded stream for sessions retained forever).
func TestRetainShimRecordsAnUnchangedConditionOnce(t *testing.T) {
	svc := stopHarness(t)
	if err := svc.Store.CreateSession(store.SessionRow{ID: "kept", State: "running", Workspace: t.TempDir()}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	shim := store.SessionShimRow{SessionID: "kept", ShimKey: "k"}
	count := func() int {
		evs, err := svc.Store.QueryEvents(store.EventFilter{SessionID: "kept", Kinds: []string{"session.shim_status"}, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	for i := 0; i < 5; i++ {
		svc.retainShim(shim, nil, "outcome_unknown")
	}
	if got := count(); got != 1 {
		t.Fatalf("shim_status events after 5 identical retains = %d, want 1", got)
	}
	svc.retainShim(shim, nil, "journal_mismatch")
	if got := count(); got != 2 {
		t.Fatalf("a changed reason must be recorded: events=%d", got)
	}
}
