package workerhub_test

import (
	"context"
	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSmokeS1(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	token, err := ids.Mint(ctx, identity.Principal{ID: "msg://device/smoke-worker", Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	hub := workerhub.New(db.DB())
	daemonServer := &daemon.Server{Identity: ids, Workers: hub, Config: daemon.Config{RemoteListener: daemon.RemoteListenerConfig{AllowedHosts: []string{"worker.test"}}}}
	server := httptest.NewServer(daemonServer.RemoteHandler("worker.test"))
	defer server.Close()
	header := http.Header{}
	header.Set("Host", "worker.test")
	header.Set("Authorization", "Bearer "+token)
	header.Set("Tether-Protocol", "1")
	ws, responseHTTP, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+workerwire.ConnectPath, header)
	if err != nil {
		if responseHTTP != nil {
			b, _ := io.ReadAll(responseHTTP.Body)
			t.Logf("HTTP %d: %s", responseHTTP.StatusCode, b)
		}
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(workerwire.Frame{T: "hello", Proto: 1, Worker: &workerwire.Worker{Label: "fake-worker", Platform: "linux", Arch: "amd64"}}); err != nil {
		t.Fatal(err)
	}
	var welcome workerwire.Frame
	if err := ws.ReadJSON(&welcome); err != nil {
		t.Fatal(err)
	}
	if welcome.T != "welcome" || welcome.WorkerID == "" {
		t.Fatalf("bad welcome: %+v", welcome)
	}
	workers, err := hub.List(ctx)
	if err != nil || len(workers) != 1 || !workers[0].Connected || workers[0].Label != "fake-worker" {
		t.Fatalf("workers: %+v, %v", workers, err)
	}
	go func() {
		var command workerwire.Frame
		if ws.ReadJSON(&command) == nil {
			_ = ws.WriteJSON(workerwire.Frame{T: "launched", CmdID: command.CmdID, Session: command.Session, PID: 123})
		}
	}()
	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	response, err := hub.Send(wait, welcome.WorkerID, workerwire.Frame{T: "launch", CmdID: "smoke-command", Session: "smoke-session"})
	if err != nil || response.PID != 123 {
		t.Fatalf("command: %+v, %v", response, err)
	}
	t.Log("fake worker authenticated, connected, listed, and answered command")
}
