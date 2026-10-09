package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/store"
)

func reaperDuration(s string) *string { return &s }

func TestReaperReasonActivityAndNoSilentCeiling(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(13 * time.Hour)
	d := launchprofile.LifecycleDurations{IdleTimeout: time.Minute}
	if got := reaperReason(now, start, now.Add(-time.Second), d, false); got != "" {
		t.Fatalf("active old session reaped: %s", got)
	}
	if got := reaperReason(now, start, now.Add(-time.Minute), d, false); got != "idle_timeout" {
		t.Fatalf("idle cause=%s", got)
	}
	d.MaxDuration = 12 * time.Hour
	if got := reaperReason(now, start, now, d, true); got != "max_duration" {
		t.Fatalf("precedence=%s", got)
	}
	d.MaxDuration = 0
	if got := reaperReason(now, start, now, d, true); got != "lease_expired" {
		t.Fatalf("lease cause=%s", got)
	}
	if got := reaperReason(now, start, start, launchprofile.LifecycleDurations{}, false); got != "" {
		t.Fatalf("implicit 12h limit: %s", got)
	}
}

func TestReaperIdlePIDZeroOutcome(t *testing.T) {
	svc := stopHarness(t)
	rt, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	startSession(t, svc, "idle", rt)
	p := &launch.Plan{Lifecycle: &launchprofile.LifecyclePolicy{IdleTimeout: reaperDuration("1m")}}
	raw, _ := json.Marshal(p)
	if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(raw), "idle"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepSessionReaper(context.Background(), time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	waitExit(t, svc, "idle")
	evs, err := svc.Store.QueryEvents(store.EventFilter{SessionID: "idle", Kinds: []string{sessionReaperEvent}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) < 2 {
		t.Fatalf("missing durable intent/outcome: %+v", evs)
	}
	if ev := terminalEvent(t, svc, "idle"); ev.Reason != "idle_timeout" {
		t.Fatalf("terminal cause=%q", ev.Reason)
	}
}

func TestReaperOrphanLeavesWorkspaceAndManagedSession(t *testing.T) {
	svc := stopHarness(t)
	rt, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	startSession(t, svc, "healthy", rt)
	t.Cleanup(func() { _ = svc.StopSession("healthy") })
	if err := svc.Store.CreateSession(store.SessionRow{ID: "missing", State: "running", Workspace: t.TempDir()}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepSessionReaper(context.Background(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession("missing")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "orphaned" || row.Workspace == "" || row.EndedAt.Valid {
		t.Fatalf("lost session=%+v", row)
	}
	row, err = svc.Store.GetSession("healthy")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "running" {
		t.Fatalf("managed PID-zero session=%s", row.State)
	}
}

func TestReaperStartStopJoined(t *testing.T) {
	svc := stopHarness(t)
	svc.StopSessionReaper()
	svc.StartSessionReaper(context.Background())
	svc.StartSessionReaper(context.Background())
	done := make(chan struct{})
	go func() { svc.StopSessionReaper(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
	svc.StopSessionReaper()
}

func TestLifecycleAgentOverrideDoesNotMaskLaunch(t *testing.T) {
	svc := buildTestService(t, map[string]launchprofile.LaunchProfile{"test-agent": {ID: "test-agent", Lifecycle: &launchprofile.LifecyclePolicy{IdleTimeout: reaperDuration("1m")}}}, t.TempDir())
	plan := basePlan()
	plan.Lifecycle = &launchprofile.LifecyclePolicy{IdleTimeout: reaperDuration("2m"), MaxDuration: reaperDuration("3h")}
	if err := svc.applyAgentOps(plan, CreateSessionInput{AgentInline: `{"name":"changed"}`, Override: `{"lifecycle":{"max_duration":"0s"}}`}); err != nil {
		t.Fatal(err)
	}
	d, err := plan.Lifecycle.Durations()
	if err != nil {
		t.Fatal(err)
	}
	if d.IdleTimeout != 2*time.Minute || d.MaxDuration != 0 {
		t.Fatalf("resolved=%+v", d)
	}
}
