package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
)

// The whole launch path with nothing faked: the CLI's client presents operator.token to the real API behind the
// real, enforcing identity middleware, the daemon's service adapter hands the request to a real app.Service built
// from a catalog, and the session row, the stored plan and the agent environment all carry the task id.
func TestLaunchTaskStampEndToEndStoresAndPlants(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, repo := t.TempDir(), t.TempDir()
	sessionRoot := t.TempDir()
	for path, body := range map[string]string{
		"global.yaml":             "version: 0.1.0\ncatalog:\n  defaults:\n    state_db: " + filepath.Join(root, "state.db") + "\n",
		"projects/p.yaml":         "id: p\nname: P\nrepo_root: " + repo + "\nworkspace:\n  default_mode: shared\n  session_root: " + sessionRoot + "\n",
		"providers/fill.yaml":     "id: fill-cli\ntype: cli\nprovider: claude\nruntime_kind: pty\ncommand: " + filepath.Join(root, "fake-provider") + "\n",
		"agents/fill-worker.yaml": "id: fill-worker\nname: Fill Worker\n",
		"launches/fill.yaml":      "id: fill-launch\nproject: p\nagent: fill-worker\nprovider: fill-cli\n",
		"fake-provider":           "#!/bin/sh\nexec sleep 60\n",
		"providers/stub.yaml":     "id: api-stub\ntype: api\n", // runs in-process; no provider binary or daemon
		"agents/worker.yaml":      "id: worker\nname: Worker\n",
		"launches/l1.yaml":        "id: l1\nproject: p\nagent: worker\nprovider: api-stub\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "fake-provider"), 0700); err != nil {
		t.Fatal(err)
	}
	svc, err := app.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	const launchID = "l1"
	ids := identity.NewStore(svc.Store.DB())
	tokenPath := filepath.Join(t.TempDir(), "operator.token")
	if err := ids.EnsureOperator(context.Background(), tokenPath); err != nil {
		t.Fatal(err)
	}
	token, err := identity.ReadTokenFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(identity.MiddlewareWithScopes(identity.Enforce, ids, nil, identity.ScopePolicy{}, api.NewHandler(api.Deps{Service: &serviceAdapter{svc: svc}})))
	t.Cleanup(srv.Close)
	c := client.New("tcp:"+srv.URL[len("http://"):], client.WithToken(token))

	const id = "CW-20261016-0155"
	res, err := c.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: launchID, TaskID: id, RunID: "42", WorkstreamID: "01M7VQ2QWYB5RZR86W2BWG8PW3", IdempotencyKey: "workstream-create"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession(res.ID)
	if err != nil || !row.RunID.Valid || row.RunID.String != "42" || !row.TaskID.Valid || row.TaskID.String != id || row.RequestingPrincipal.String != identity.OperatorID {
		t.Fatalf("session row task = %+v err=%v, want %q", row.TaskID, err, id)
	}
	plan, err := svc.Store.GetLaunchPlan(res.ID)
	if err != nil || plan.RunID != "42" || plan.Env[app.RunIDEnv] != "42" || plan.TaskID != id || plan.Env[app.TaskIDEnv] != id {
		t.Fatalf("stored plan task = %q env = %q err=%v", plan.TaskID, plan.Env[app.TaskIDEnv], err)
	}
	if row.WorkstreamID.String != "01M7VQ2QWYB5RZR86W2BWG8PW3" || plan.WorkstreamID != row.WorkstreamID.String {
		t.Fatal("external workstream not stamped")
	}
	replay, err := c.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: launchID, TaskID: id, RunID: "42", WorkstreamID: row.WorkstreamID.String, IdempotencyKey: "workstream-create"})
	if err != nil || !replay.Replayed || replay.ID != res.ID {
		t.Fatalf("create replay: %+v %v", replay, err)
	}
	_, err = c.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: launchID, TaskID: id, RunID: "42", WorkstreamID: "019a9e45-6789-7123-8123-0123456789ab", IdempotencyKey: "workstream-create"})
	if err == nil {
		t.Fatal("changed workstream replay accepted")
	}
	after, err := svc.Store.GetSession(res.ID)
	if err != nil || after.WorkstreamID != row.WorkstreamID {
		t.Fatal("replay changed stamp")
	}
	// A bare keyed create can receive its missing stamps on the split launch.
	fillInput := api.LaunchRequest{Launch: "fill-launch", IdempotencyKey: "fill-at-launch"}
	unstamped, err := c.CreateSessionWithInput(context.Background(), fillInput)
	if err != nil {
		t.Fatal(err)
	}
	filled, err := c.LaunchSessionWithInput(context.Background(), unstamped.ID, api.SessionLaunchRequest{TaskID: id, WorkstreamID: "019a9e45-6789-7123-8123-0123456789ab"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.StopSession(filled.ID) })
	filledRow, err := svc.Store.GetSession(filled.ID)
	if err != nil || filledRow.TaskID.String != id || filledRow.WorkstreamID.String != "019a9e45-6789-7123-8123-0123456789ab" {
		t.Fatal("split HTTP launch lost stamps")
	}
	replayFill, err := c.CreateSessionWithInput(context.Background(), fillInput)
	if err != nil || !replayFill.Replayed || replayFill.ID != filled.ID {
		t.Fatalf("keyed create replay after fill: %+v %v", replayFill, err)
	}
	if _, err := c.LaunchSessionWithInput(context.Background(), filled.ID, api.SessionLaunchRequest{TaskID: id, WorkstreamID: filledRow.WorkstreamID.String}); err != nil {
		t.Fatal("matching keyed launch replay failed", err)
	}
	if _, err := c.LaunchSessionWithInput(context.Background(), filled.ID, api.SessionLaunchRequest{WorkstreamID: "different"}); err == nil {
		t.Fatal("running session restamped")
	}
	afterFill, err := svc.Store.GetSession(filled.ID)
	if err != nil || afterFill.TaskID != filledRow.TaskID || afterFill.WorkstreamID != filledRow.WorkstreamID {
		t.Fatal("replay changed launch fill")
	}
	// An unkeyed, bare create must also preserve the verified creator through
	// the daemon adapter, then a relaunch must retain its lineage and creator.
	bare, err := c.CreateSession(context.Background(), launchID)
	if err != nil {
		t.Fatal(err)
	}
	bareRow, err := svc.Store.GetSession(bare.ID)
	if err != nil || bareRow.RequestingPrincipal.String != identity.OperatorID || bareRow.Intent != "fresh" {
		t.Fatalf("bare create provenance: %+v %v", bareRow, err)
	}
	continued, err := c.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: launchID, Continuity: "relaunch"})
	if err != nil {
		t.Fatal(err)
	}
	continuedRow, err := svc.Store.GetSession(continued.ID)
	if err != nil || continuedRow.RequestingPrincipal.String != identity.OperatorID || continuedRow.Intent != "resume" || continuedRow.ParentSessionID.String != bare.ID {
		t.Fatalf("relaunch provenance: %+v %v", continuedRow, err)
	}
	// Without a task id nothing is stamped.
	plain, err := c.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: launchID, PromptAppend: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if prow, err := svc.Store.GetSession(plain.ID); err != nil || prow.TaskID.Valid || prow.RunID.Valid || prow.WorkstreamID.Valid {
		t.Fatalf("unstamped session task = %+v err=%v", prow.TaskID, err)
	}
}
