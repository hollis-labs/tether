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
	for path, body := range map[string]string{
		"global.yaml":         "version: 0.1.0\ncatalog:\n  defaults:\n    state_db: " + filepath.Join(root, "state.db") + "\n",
		"projects/p.yaml":     "id: p\nname: P\nrepo_root: " + repo + "\nworkspace:\n  default_mode: shared\n  session_root: " + filepath.Join(root, "sessions") + "\n",
		"providers/stub.yaml": "id: api-stub\ntype: api\n", // runs in-process; no provider binary or daemon
		"agents/worker.yaml":  "id: worker\nname: Worker\n",
		"launches/l1.yaml":    "id: l1\nproject: p\nagent: worker\nprovider: api-stub\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
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
	res, err := c.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: launchID, TaskID: id})
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession(res.ID)
	if err != nil || !row.TaskID.Valid || row.TaskID.String != id {
		t.Fatalf("session row task = %+v err=%v, want %q", row.TaskID, err, id)
	}
	plan, err := svc.Store.GetLaunchPlan(res.ID)
	if err != nil || plan.TaskID != id || plan.Env[app.TaskIDEnv] != id {
		t.Fatalf("stored plan task = %q env = %q err=%v", plan.TaskID, plan.Env[app.TaskIDEnv], err)
	}
	// Without a task id nothing is stamped.
	plain, err := c.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: launchID, PromptAppend: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if prow, err := svc.Store.GetSession(plain.ID); err != nil || prow.TaskID.Valid {
		t.Fatalf("unstamped session task = %+v err=%v", prow.TaskID, err)
	}
}
