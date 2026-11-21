package app

import (
	"context"
	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/identity"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
)

func TestTeamSessionStampsCorrelationAndRunToken(t *testing.T) {
	for _, k := range []string{"TETHER_TASK_ID", "TETHER_WORKSTREAM_ID", "TETHER_PROJECT_ID", "TETHER_RUN_ID"} {
		t.Setenv(k, "ambient-wrong")
	}

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
	var actualEnv []string
	svc.factories = map[string]RuntimeFactory{"stub": func(p *launch.Plan) (agentsessions.Runtime, error) {
		r, err := stub.New(p)
		return correlationCaptureRuntime{Runtime: r, env: &actualEnv}, err
	}}

	l, err := svc.CreateTeamSessionWithContext(context.Background(), "team:correlation", "l1", TeamBootContext{RunID: "team-run", Slot: "workers", TaskID: "CW-20261016-0155", WorkstreamID: "01M7VQ2QWYB5RZR86W2BWG8PW3"})
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

	if row.WorkstreamID.String != "01M7VQ2QWYB5RZR86W2BWG8PW3" || stored.WorkstreamID != row.WorkstreamID.String || stored.Env["TETHER_WORKSTREAM_ID"] != row.WorkstreamID.String {
		t.Fatal("workstream not persisted before launch")
	}
	if _, err := svc.LaunchSession(l.SessionID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.StopSession(l.SessionID) })
	for _, want := range []string{"TETHER_TASK_ID=CW-20261016-0155", "TETHER_PROJECT_ID=p", "TETHER_WORKSTREAM_ID=01M7VQ2QWYB5RZR86W2BWG8PW3"} {
		if !strings.Contains(strings.Join(actualEnv, "\n"), want) {
			t.Fatalf("missing %s", want)
		}
	}
	env := map[string]string{}
	for _, v := range actualEnv {
		k, v, _ := strings.Cut(v, "=")
		env[k] = v
	}
	principal, err := identity.NewStore(svc.Store.DB()).Verify(context.Background(), env["TETHER_TOKEN"])
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.VerifyRunToken(testRunProof(t, env), principal)
	if err != nil || claims.Task != row.TaskID.String || claims.Workstream != row.WorkstreamID.String {
		t.Fatal("minted run token lost task/workstream binding")
	}
}
