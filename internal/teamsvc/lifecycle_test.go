package teamsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
)

// finalByWorkers makes the "workers" slot the final reporter for these tests.
type finalByWorkers struct{ ceilings }

func (finalByWorkers) DissolvesOnResult(_ teams.Team, slot string) bool { return slot == "workers" }
func (finalByWorkers) ManagesLifecycle(teams.Team) bool                 { return true }

type deadlines struct {
	mu   sync.Mutex
	open map[string]*RunDeadline
}

func newDeadlines() *deadlines { return &deadlines{open: map[string]*RunDeadline{}} }
func (d *deadlines) RecordRunDeadline(_ context.Context, id string, at time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.open[id] == nil {
		d.open[id] = &RunDeadline{RunID: id, Deadline: at}
	}
	return nil
}
func (d *deadlines) RequestRunEnd(_ context.Context, id, reason string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.open[id]
	if r == nil {
		return false, nil
	}
	if r.EndReason == "" {
		r.EndReason = reason
	}
	return true, nil
}
func (d *deadlines) OpenRuns(context.Context, int) ([]RunDeadline, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []RunDeadline
	for _, r := range d.open {
		out = append(out, *r)
	}
	return out, nil
}
func (d *deadlines) GetRunDeadline(_ context.Context, id string) (time.Time, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.open[id]; r != nil {
		return r.Deadline, true, nil
	}
	return time.Time{}, false, nil
}
func (d *deadlines) CloseRun(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.open, id)
	return nil
}

func noLiveMembers(t *testing.T, f *fixture) {
	t.Helper()
	roster, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	for _, m := range roster.Members {
		if m.Status == "active" || m.Status == "provisioning" {
			t.Fatalf("member %s still %s after dissolve", m.ID, m.Status)
		}
	}
}

func reportFinal(t *testing.T, f *fixture) {
	t.Helper()
	task, err := f.svc.Delegate(caller(verified(ownerURN)), MessageRequest{Key: "task", RunID: "run", Address: string(workerURN), Body: "work"})
	must(t, err)
	_, err = f.svc.ReportResult(caller(verified(workerURN)), ReportResultRequest{Key: "final", RunID: "run", DelegateKey: task.DeliveryKeys[0], Body: "done"})
	must(t, err)
}

func TestFinalResultDissolvesInlineWithoutDeadlineStore(t *testing.T) {
	f := fixtureNew(t)
	f.svc.deps.Ceilings = finalByWorkers{ceilings{policy: policy()}}
	reportFinal(t, f)
	noLiveMembers(t, f)
}

func TestFinalResultThenReaperDissolvesAndTimeoutAndAllStopped(t *testing.T) {
	setup := func(t *testing.T) (*fixture, *deadlines) {
		f := fixtureNew(t)
		f.svc.deps.Ceilings = finalByWorkers{ceilings{policy: policy()}}
		d := newDeadlines()
		f.svc.deps.Deadlines = d
		f.host.NowTime = time.Unix(1000, 0)
		must(t, d.RecordRunDeadline(context.Background(), "run", f.host.NowTime.Add(time.Hour)))
		return f, d
	}
	t.Run("final result", func(t *testing.T) {
		f, d := setup(t)
		reportFinal(t, f)
		// The call returns first; the run is asked to end and still has members.
		if d.open["run"].EndReason != EndResult {
			t.Fatalf("end not requested: %+v", d.open["run"])
		}
		must(t, f.svc.ReapRuns(context.Background(), 10))
		noLiveMembers(t, f)
		if len(d.open) != 0 {
			t.Fatal("run still open after dissolve")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f, d := setup(t)
		must(t, f.svc.ReapRuns(context.Background(), 10))
		if len(d.open) != 1 {
			t.Fatal("run reaped before its deadline")
		}
		f.host.NowTime = f.host.NowTime.Add(time.Hour)
		must(t, f.svc.ReapRuns(context.Background(), 10))
		noLiveMembers(t, f)
	})
	t.Run("all members stopped", func(t *testing.T) {
		f, d := setup(t)
		must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error {
			for i, m := range r.Members {
				if m.Governance != teams.Owner {
					r.Members[i].Status = "stopped"
				}
			}
			return nil
		}))
		must(t, f.svc.ReapRuns(context.Background(), 10))
		if len(d.open) != 0 {
			t.Fatal("run with no live member was not ended")
		}
	})
}

func TestFormationDefaultsFillUnspecifiedLimitsAndRequestsOnlyTighten(t *testing.T) {
	ceiling := mesh.Limits{MaxDepth: 3, MaxChildren: 8, FanOut: 8, Budget: 1000, Timeout: 12 * time.Hour, MaxRounds: 500, MaxStalls: 40}
	def := mesh.Limits{MaxDepth: 2, MaxChildren: 6, FanOut: 6, Budget: 100, Timeout: 4 * time.Hour, MaxRounds: 200, MaxStalls: 20}
	got := withDefaults(mesh.Limits{Timeout: time.Hour}, def)
	if got.Timeout != time.Hour || got.MaxRounds != 200 || got.MaxStalls != 20 || got.MaxDepth != 2 || got.Budget != 100 {
		t.Fatalf("defaults not filled: %+v", got)
	}
	resolved, err := teams.ResolveLimits(got, ceiling, ceiling)
	must(t, err)
	if resolved.Timeout != time.Hour || resolved.MaxRounds != 200 {
		t.Fatalf("resolved: %+v", resolved)
	}
	if _, err = teams.ResolveLimits(withDefaults(mesh.Limits{Timeout: 13 * time.Hour}, def), ceiling, ceiling); err == nil {
		t.Fatal("a request widened the maximum timeout")
	}
}

// failingRoster fails the Mutate that would seat the forming owner.
type failingRoster struct{ teams.RosterStore }

func (r failingRoster) Mutate(ctx context.Context, run string, fn func(*teams.Roster) error) error {
	before, err := r.Snapshot(ctx, run)
	if err != nil {
		return r.RosterStore.Mutate(ctx, run, fn) // run not seeded yet
	}
	probe := before
	probe.Members = append([]teams.Member(nil), before.Members...)
	if err = fn(&probe); err != nil {
		return err
	}
	for _, m := range probe.Members {
		if m.Governance == teams.Owner {
			owned := false
			for _, old := range before.Members {
				owned = owned || (old.ID == m.ID && old.Governance == teams.Owner)
			}
			if !owned {
				return errors.New("injected owner mutate failure")
			}
		}
	}
	return r.RosterStore.Mutate(ctx, run, fn)
}

type failingDeadlines struct{ *deadlines }

func (failingDeadlines) RecordRunDeadline(context.Context, string, time.Time) error {
	return errors.New("injected deadline failure")
}

func noSessionLeft(t *testing.T, f *fixture) {
	t.Helper()
	provisioned := f.host.Provisioned()
	if len(provisioned) == 0 {
		t.Fatal("nothing was launched; the test proves nothing")
	}
	for _, m := range provisioned {
		if m.Status == "active" || m.Status == "provisioning" {
			t.Fatalf("member %s left %s after a failed formation", m.ID, m.Status)
		}
	}
}

func TestFailedFormationLeavesNoSessionRunning(t *testing.T) {
	for _, mode := range []string{"deadline", "owner mutate"} {
		t.Run(mode, func(t *testing.T) {
			f := fixtureNew(t)
			f.svc.deps.Ceilings = finalByWorkers{ceilings{policy: policy()}}
			f.svc.deps.Deadlines = newDeadlines()
			if mode == "deadline" {
				f.svc.deps.Deadlines = failingDeadlines{newDeadlines()}
			} else {
				f.svc.deps.Roster = failingRoster{f.host}
				f.svc.launcher.Roster = failingRoster{f.host}
			}
			p := verified(ownerURN)
			team := formationDefinition(p)
			team.ID = "formation"
			_, err := f.svc.Form(caller(p), FormRequest{Key: "fail", Team: team, Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 2}}})
			if err == nil || !strings.Contains(fmt.Sprint(errors.Unwrap(err), err), "injected") && !errors.Is(err, ErrNotFound) {
				t.Fatalf("injected failure not reached: %v", err)
			}
			var typed *Error
			if errors.As(err, &typed) && typed.Cause != nil && !strings.Contains(typed.Cause.Error(), "injected") {
				t.Fatalf("failed for another reason: %v", typed.Cause)
			}
			noSessionLeft(t, f)
		})
	}
}

// The deadline row is written for lifecycle-managed teams only; others behave
// exactly as before (no reaper row, no timeout stop).
func TestOnlyManagedTeamsGetDeadlines(t *testing.T) {
	for _, managed := range []bool{true, false} {
		f := fixtureNew(t)
		d := newDeadlines()
		f.svc.deps.Deadlines = d
		if managed {
			f.svc.deps.Ceilings = finalByWorkers{ceilings{policy: policy()}}
		}
		p := verified(ownerURN)
		team := formationDefinition(p)
		team.ID = "formation"
		_, err := f.svc.Form(caller(p), FormRequest{Key: "rows", Team: team})
		must(t, err)
		if got := len(d.open) == 1; got != managed {
			t.Fatalf("managed=%v rows=%d", managed, len(d.open))
		}
	}
}

// A final result with no reaper row (never recorded or already closed) still
// dissolves the run.
func TestFinalResultWithoutDeadlineRowStillDissolves(t *testing.T) {
	f := fixtureNew(t)
	f.svc.deps.Ceilings = finalByWorkers{ceilings{policy: policy()}}
	f.svc.deps.Deadlines = newDeadlines() // empty: RequestRunEnd finds no row
	reportFinal(t, f)
	noLiveMembers(t, f)
}

// trackedRuns remembers which runs were dissolved.
type trackedRuns struct {
	runs
	mu        sync.Mutex
	dissolved map[string]bool
}

func (r *trackedRuns) GetRun(ctx context.Context, id string) (teams.TeamRun, error) {
	run, err := r.runs.GetRun(ctx, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil && r.dissolved[id] {
		run.Status = "dissolved"
	}
	return run, err
}
func (r *trackedRuns) MarkDissolved(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dissolved[id] = true
	return nil
}

// A retry with the same key after a failed, torn-down formation must not
// report success with a dead run.
func TestRetryAfterTeardownDoesNotSucceedWithDeadRun(t *testing.T) {
	f := fixtureNew(t)
	f.svc.deps.Ceilings = finalByWorkers{ceilings{policy: policy()}}
	f.svc.deps.Runs = &trackedRuns{runs: runs{f.run}, dissolved: map[string]bool{}}
	bad := failingDeadlines{newDeadlines()}
	f.svc.deps.Deadlines = bad
	p := verified(ownerURN)
	team := formationDefinition(p)
	team.ID = "formation"
	req := FormRequest{Key: "retry", Team: team, Launch: teams.LaunchRequest{Counts: map[string]int{"workers": 2}}}
	if _, err := f.svc.Form(caller(p), req); err == nil {
		t.Fatal("injected failure ignored")
	}
	noSessionLeft(t, f)
	healthy := newDeadlines()
	f.svc.deps.Deadlines = healthy
	_, err := f.svc.Form(caller(p), req)
	if err == nil {
		t.Fatal("retry returned success with a torn-down run")
	}
	if len(healthy.open) != 0 {
		t.Fatal("retry recorded a deadline for a dead run")
	}
	noSessionLeft(t, f)
}
