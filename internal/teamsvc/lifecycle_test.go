package teamsvc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
)

// finalByWorkers makes the "workers" slot the final reporter for these tests.
type finalByWorkers struct{ ceilings }

func (finalByWorkers) DissolvesOnResult(_ teams.Team, slot string) bool { return slot == "workers" }

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
func (d *deadlines) RequestRunEnd(_ context.Context, id, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.open[id]; r != nil && r.EndReason == "" {
		r.EndReason = reason
	}
	return nil
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
