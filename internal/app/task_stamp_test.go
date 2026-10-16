package app

import (
	"context"
	"github.com/hollis-labs/tether/internal/api"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
)

func TestStampTaskIDOverwritesAnySpoofedEnv(t *testing.T) {
	plan := &launch.Plan{Env: map[string]string{TaskIDEnv: "evil", "KEEP": "1"}}
	stampTaskID(plan, "CW-20261016-0155")
	if plan.TaskID != "CW-20261016-0155" || plan.Env[TaskIDEnv] != "CW-20261016-0155" || plan.Env["KEEP"] != "1" {
		t.Fatalf("plan = %+v", plan)
	}
	// No task named: a spoofed variable from catalog or override env is removed, not kept.
	plan = &launch.Plan{Env: map[string]string{TaskIDEnv: "evil"}}
	stampTaskID(plan, "")
	if _, ok := plan.Env[TaskIDEnv]; ok || plan.TaskID != "" {
		t.Fatalf("spoofed env survived: %+v", plan)
	}
	stampTaskID(&launch.Plan{}, "") // nil Env must not panic
	nilEnv := &launch.Plan{}
	stampTaskID(nilEnv, "CW-1")
	if nilEnv.Env[TaskIDEnv] != "CW-1" {
		t.Fatalf("nil env not initialized: %+v", nilEnv)
	}
}

// A launch carries the launcher's task id onto the session row and into the plan env, and
// an env override in the request (or the catalog) cannot replace it.
func TestCreateSessionStampsTaskID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	svc := bindingHarness(t)
	svc.CatalogRoot = t.TempDir()
	svc.Catalog = &config.Catalog{
		Global:    config.Global{Version: "test"},
		Projects:  map[string]config.Project{"p": {ID: "p", RepoRoot: t.TempDir(), Workspace: config.WorkspaceSpec{SessionRoot: t.TempDir(), DefaultMode: "shared"}}},
		Agents:    map[string]config.Agent{"worker": {ID: "worker"}},
		Providers: map[string]config.Provider{"stub": {ID: "stub", Provider: "claude", RuntimeKind: config.RuntimeKindPTY, Command: "test-fake"}},
		Launches:  map[string]config.Launch{"l1": {ID: "l1", Project: "p", Agent: "worker", Provider: "stub"}},
	}
	svc.factories = map[string]RuntimeFactory{"stub": stub.New}

	l, err := svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "l1", TaskID: "CW-20261016-0155", Override: `{"env":{"TETHER_TASK_ID":"evil"}}`})
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession(l.SessionID)
	if err != nil || !row.TaskID.Valid || row.TaskID.String != "CW-20261016-0155" {
		t.Fatalf("row task = %+v err=%v", row.TaskID, err)
	}
	if l.Plan.TaskID != "CW-20261016-0155" || l.Plan.Env[TaskIDEnv] != "CW-20261016-0155" {
		t.Fatalf("plan task = %q env = %q", l.Plan.TaskID, l.Plan.Env[TaskIDEnv])
	}
	stored, err := svc.Store.GetLaunchPlan(l.SessionID)
	if err != nil || stored.TaskID != "CW-20261016-0155" || stored.Env[TaskIDEnv] != "CW-20261016-0155" {
		t.Fatalf("stored plan = %+v err=%v", stored, err)
	}

	// No task named, but an override tries to name one through env: nothing is stamped.
	l2, err := svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "l1", Override: `{"env":{"TETHER_TASK_ID":"evil"}}`})
	if err != nil {
		t.Fatal(err)
	}
	row2, err := svc.Store.GetSession(l2.SessionID)
	if err != nil || row2.TaskID.Valid {
		t.Fatalf("row task = %+v err=%v, want none", row2.TaskID, err)
	}
	if v, ok := l2.Plan.Env[TaskIDEnv]; ok {
		t.Fatalf("spoofed TETHER_TASK_ID survived: %q", v)
	}
}

// An Outpost launch gets the stamp through the LaunchSpec env: workerSpec copies the plan env.
func TestWorkerSpecCarriesTaskIDEnv(t *testing.T) {
	s := &Service{Catalog: &config.Catalog{}}
	plan := &launch.Plan{ProviderBrand: "claude"}
	stampTaskID(plan, "CW-20261016-0155")
	spec, err := s.workerSpec(context.Background(), "sess-1", plan)
	if err != nil || spec.Env[TaskIDEnv] != "CW-20261016-0155" {
		t.Fatalf("spec env = %v err=%v", spec.Env, err)
	}
}

// A resume continues the same work, so the child session keeps the parent's task stamp.
func TestResumeKeepsTaskStamp(t *testing.T) {
	rig := newCodexRig(t)
	parent, err := rig.svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "codex-launch", TaskID: "CW-20261016-0155"})
	if err != nil {
		t.Fatal(err)
	}
	owned := []string{parent.SessionID}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for _, id := range owned {
			if _, managed := rig.svc.Manager.Get(id); managed {
				_ = rig.svc.Manager.Stop(ctx, id)
				_, _ = rig.svc.Manager.WaitSession(ctx, id)
			}
			for {
				if _, pending := rig.svc.turnOutputs.Load(id); !pending {
					break
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
					return
				}
			}
		}
	})
	if _, err := rig.svc.LaunchSession(parent.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.StopSession(parent.SessionID); err != nil {
		t.Fatal(err)
	}
	rig.ended(parent.SessionID)
	rig.checkpoint("resume-task", parent.SessionID, "")
	resumed, err := rig.svc.ResumeLogicalAgent("agent", api.ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owned = append(owned, resumed.SessionID)
	row, err := rig.svc.Store.GetSession(resumed.SessionID)
	if err != nil || row.TaskID.String != "CW-20261016-0155" {
		t.Fatalf("resumed task = %+v err=%v, want the parent's", row.TaskID, err)
	}
	plan, err := rig.svc.Store.GetLaunchPlan(resumed.SessionID)
	if err != nil || plan.Env[TaskIDEnv] != "CW-20261016-0155" {
		t.Fatalf("resumed plan env = %q err=%v", plan.Env[TaskIDEnv], err)
	}
}
