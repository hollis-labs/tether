package app

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSmokeS3(t *testing.T) {
	binary := os.Getenv("WORKER_SMOKE_BINARY")
	if binary == "" {
		t.Skip("run scripts/worker-smoke-s3.sh")
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hub := workerhub.New(db.DB())
	defer hub.Close()
	ids := identity.NewStore(db.DB())
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = (&daemon.Server{Identity: ids, Workers: hub}).RemoteHandler(server.Listener.Addr().String())
	server.Start()
	defer server.Close()
	grant, err := ids.CreatePairingGrant(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}, Verified: true}, "s3", []string{identity.ScopeWorker}, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "tw-s3-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := workerruntime.Enroll(context.Background(), root, server.URL, grant.Code, "s3"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := workerruntime.Load(root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	w := &workerruntime.Worker{Root: root, Config: cfg, Version: "smoke", Caps: func(context.Context) json.RawMessage { return json.RawMessage(`{}`) }, OnConnect: func() { ready <- struct{}{} }}
	executor := workerruntime.NewExecutor(root, binary, w.Send)
	w.Command = executor.Command
	w.Inventory = executor.Inventory
	go func() { _ = w.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("connect timeout")
	}
	workerID, err := hub.Resolve(ctx, "s3")
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "hub-session")
	if err = os.MkdirAll(filepath.Join(workspace, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	plan := &launch.Plan{ProviderID: "echo", ProviderBrand: "sh", LogicalAgentID: "echo-worker", Command: "/hub-only/not-a-binary", Args: []string{"-c", "echo ready; while IFS= read -r line; do echo \"$line\"; done"}, BootPrompt: "smoke-boot", BootMode: "stdin"}
	if err := db.CreateSession(store.SessionRow{ID: "smoke-s3", WorkerID: workerID, LogicalAgentID: plan.LogicalAgentID, ProviderID: plan.ProviderID, ProviderKind: "cli", Workspace: workspace, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Workers: hub, Catalog: &config.Catalog{}}
	if _, err := svc.LaunchSessionWithContext(ctx, "smoke-s3"); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetSession("smoke-s3")
	if err != nil || row.State != "running" || row.WorkerID != workerID {
		t.Fatalf("hub row %+v: %v", row, err)
	}
	if err := svc.StopSession("smoke-s3"); err != nil {
		t.Fatal(err)
	}
	t.Log("hub-owned session launched a real detached shim on outpost; hub binary path ignored; stop delivered")
}
