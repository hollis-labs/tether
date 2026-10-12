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
	"github.com/hollis-labs/tether/internal/workerwire"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSmokeS3(t *testing.T) {
	workerSmoke(t, 3)
}
func TestSmokeS4(t *testing.T) { workerSmoke(t, 4) }
func workerSmoke(t *testing.T, stage int) {
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
	ready := make(chan struct{}, 4)
	w := &workerruntime.Worker{Root: root, Config: cfg, Version: "smoke", Caps: func(context.Context) json.RawMessage { return json.RawMessage(`{}`) }, OnConnect: func() { ready <- struct{}{} }}
	executor := workerruntime.NewExecutor(root, binary, w.Send)
	var children []*exec.Cmd
	executor.Spawn = func(path string) (*exec.Cmd, error) {
		child := exec.Command(binary, "shim-host", "--launch", path)
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err := child.Start()
		if err == nil {
			children = append(children, child)
		}
		return child, err
	}
	defer func() {
		for _, child := range children {
			_ = child.Process.Signal(syscall.SIGTERM)
		}
	}()
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
	plan := &launch.Plan{ProviderID: "echo", ProviderBrand: "sh", LogicalAgentID: "echo-worker", Command: "/hub-only/not-a-binary", Args: []string{"-c", "echo ready; for n in 1 2 3 4 5; do sleep 0.1; echo tick-$n; done; while IFS= read -r line; do echo \"$line\"; done"}, BootPrompt: "smoke-boot", BootMode: "stdin"}
	if err := db.CreateSession(store.SessionRow{ID: "smoke-s3", WorkerID: workerID, LogicalAgentID: plan.LogicalAgentID, ProviderID: plan.ProviderID, ProviderKind: "cli", Workspace: workspace, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Workers: hub, Catalog: &config.Catalog{}}
	hub.Event = svc.ingestWorkerFrame
	if _, err := svc.LaunchSessionWithContext(ctx, "smoke-s3"); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetSession("smoke-s3")
	if err != nil || row.State != "running" || row.WorkerID != workerID {
		t.Fatalf("hub row %+v: %v", row, err)
	}
	if stage >= 4 {
		path := filepath.Join(workspace, "logs", "session.log")
		deadline := time.Now().Add(3 * time.Second)
		for {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "ready") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("missing live output")
			}
			time.Sleep(20 * time.Millisecond)
		}
		hub.Close()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("reconnect timeout")
		}
		deadline = time.Now().Add(3 * time.Second)
		var output []byte
		for {
			output, _ = os.ReadFile(path)
			if strings.Contains(string(output), "tick-5") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("missing replay: %s", output)
			}
			time.Sleep(20 * time.Millisecond)
		}
		for _, line := range []string{"tick-1\n", "tick-2\n", "tick-3\n", "tick-4\n", "tick-5\n"} {
			if strings.Count(string(output), line) != 1 {
				t.Fatalf("replay gap/duplicate: %s", output)
			}
		}
		if err := svc.ingestWorkerFrame(workerID, workerwire.Frame{T: "output", Session: "smoke-s3", Seq: 1, Data: []byte("DUPLICATE")}); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(output) {
			t.Fatal("duplicate changed session log")
		}
		t.Log("live output and reconnect replay are gap-free and deduplicated; shim kept running")
	}
	if err := svc.StopSession("smoke-s3"); err != nil {
		t.Fatal(err)
	}
	t.Log("hub-owned session launched a real detached shim on outpost; hub binary path ignored; stop delivered")
}
