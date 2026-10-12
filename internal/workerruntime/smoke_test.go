package workerruntime_test

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSmokeS2(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	hub := workerhub.New(db.DB())
	defer hub.Close()
	server := httptest.NewUnstartedServer(nil)
	ds := &daemon.Server{Identity: ids, Workers: hub}
	server.Config.Handler = ds.RemoteHandler(server.Listener.Addr().String())
	server.Start()
	defer server.Close()
	grant, err := ids.CreatePairingGrant(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}}, "smoke-outpost", []string{identity.ScopeWorker}, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "worker")
	if err := workerruntime.Enroll(context.Background(), root, server.URL, grant.Code, "smoke-outpost"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "device.token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("device token must be 0600", err)
	}
	cfg, err := workerruntime.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := make(chan struct{}, 2)
	done := make(chan error, 1)
	w := &workerruntime.Worker{Root: root, Config: cfg, Version: "smoke", Caps: func(context.Context) json.RawMessage { return json.RawMessage(`{"smoke":true}`) }, OnConnect: func() { connected <- struct{}{} }}
	go func() { done <- w.Run(ctx) }()
	wait := func() {
		t.Helper()
		select {
		case <-connected:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not connect")
		}
	}
	wait()
	workers, err := hub.List(ctx)
	if err != nil || len(workers) != 1 || !workers[0].Connected {
		t.Fatal("outpost missing", err)
	}
	hub.Close()
	wait()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
	t.Log("real pairing exchange, private token, hello capabilities, and reconnect passed")
}
