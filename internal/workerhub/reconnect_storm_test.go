package workerhub_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// stormWorker is a fake worker whose connections hold live sessions.
type stormWorker struct {
	t      *testing.T
	url    string
	header http.Header
}

func newStormWorker(t *testing.T, hub *workerhub.Hub, db *store.Store) *stormWorker {
	t.Helper()
	ids := identity.NewStore(db.DB())
	expires := time.Now().Add(time.Hour)
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://device/storm-worker", Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&daemon.Server{Identity: ids, Workers: hub, Config: daemon.Config{RemoteListener: daemon.RemoteListenerConfig{AllowedHosts: []string{"worker.test"}}}}).RemoteHandler("worker.test"))
	t.Cleanup(server.Close)
	header := http.Header{}
	header.Set("Host", "worker.test")
	header.Set("Authorization", "Bearer "+token)
	header.Set("Tether-Protocol", "1")
	return &stormWorker{t: t, url: "ws" + strings.TrimPrefix(server.URL, "http") + workerwire.ConnectPath, header: header}
}

// connect says hello holding sessions and returns the connection and its welcome.
func (w *stormWorker) connect(sessions ...string) (*websocket.Conn, workerwire.Frame) {
	w.t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(w.url, w.header)
	if err != nil {
		w.t.Fatal(err)
	}
	held := make([]workerwire.Session, 0, len(sessions))
	for _, id := range sessions {
		held = append(held, workerwire.Session{ID: id, State: "running"})
	}
	if err = ws.WriteJSON(workerwire.Frame{T: "hello", Proto: 1, Sessions: held, Worker: &workerwire.Worker{Label: "storm-worker", Platform: "linux", Arch: "amd64"}}); err != nil {
		w.t.Fatal(err)
	}
	var welcome workerwire.Frame
	if err = ws.ReadJSON(&welcome); err != nil || welcome.T != "welcome" {
		w.t.Fatalf("welcome: %+v %v", welcome, err)
	}
	return ws, welcome
}

// replays reads frames until every session in want has been asked to replay.
func replays(t *testing.T, ws *websocket.Conn, want ...string) {
	t.Helper()
	seen := map[string]bool{}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = ws.SetReadDeadline(time.Time{}) }()
	for len(seen) < len(want) {
		var f workerwire.Frame
		if err := ws.ReadJSON(&f); err != nil {
			t.Fatalf("waiting for replays of %v (have %v): %v", want, seen, err)
		}
		if f.T == "replay" && slices.Contains(want, f.Session) {
			seen[f.Session] = true
		}
	}
}

// CW-20261114-0168: during a database stall the hub's handshake lookups of the
// sessions a reconnecting worker held failed, and a failed lookup put the
// session on the welcome's kill list: 26 live sessions were SIGTERMed. A
// reconnect storm whose lookups all fail must order no kill; the sessions are
// left running and replayed.
func TestReconnectStormNeverKillsOnLookupError(t *testing.T) {
	hub, db := openHub(t)
	var mu sync.Mutex
	var ordered []string
	hub.KillOrdered = func(_, session, reason string) {
		mu.Lock()
		defer mu.Unlock()
		ordered = append(ordered, session+":"+reason)
	}
	workerhub.SetLookupHeld(hub, func(context.Context, string, string) (string, int64, error) {
		return "", 0, context.DeadlineExceeded
	})
	w := newStormWorker(t, hub, db)
	live := []string{"live-1", "live-2", "live-3", "live-4", "live-5"}
	for range 5 {
		ws, welcome := w.connect(live...)
		if len(welcome.Kill) != 0 || len(welcome.Resume) != 0 {
			t.Fatalf("welcome with failed lookups: kill %v resume %v", welcome.Kill, welcome.Resume)
		}
		replays(t, ws, live...)
		_ = ws.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ordered) != 0 {
		t.Fatalf("kills ordered: %v", ordered)
	}
}

// A kill needs a definite answer: terminal here, or no such session on this
// worker. Each one is reported with its reason before the welcome goes out.
func TestWelcomeKillsOnlyOnADefiniteAnswer(t *testing.T) {
	hub, db := openHub(t)
	var mu sync.Mutex
	ordered := map[string]string{}
	hub.KillOrdered = func(_, session, reason string) {
		mu.Lock()
		defer mu.Unlock()
		ordered[session] = reason
	}
	workerhub.SetLookupHeld(hub, func(_ context.Context, session, _ string) (string, int64, error) {
		switch session {
		case "s-killed":
			return "killed", 0, nil
		case "s-gone":
			return "", 0, sql.ErrNoRows
		case "s-live":
			return "running", 4, nil
		}
		return "", 0, errors.New("database is locked")
	})
	ws, welcome := newStormWorker(t, hub, db).connect("s-killed", "s-gone", "s-live", "s-unreadable")
	defer func() { _ = ws.Close() }()
	slices.Sort(welcome.Kill)
	if !slices.Equal(welcome.Kill, []string{"s-gone", "s-killed"}) {
		t.Fatalf("kill = %v", welcome.Kill)
	}
	if len(welcome.Resume) != 1 || welcome.Resume[0].Session != "s-live" || welcome.Resume[0].FromSeq != 5 {
		t.Fatalf("resume = %+v", welcome.Resume)
	}
	mu.Lock()
	if ordered["s-killed"] != "hub_state_killed" || ordered["s-gone"] != "unknown_to_hub" || len(ordered) != 2 {
		t.Fatalf("kill reasons = %v", ordered)
	}
	mu.Unlock()
	replays(t, ws, "s-unreadable")
}

// End to end: a database stall longer than the device-stream verify timeout no
// longer drops a connected worker (the 10-60s reconnect loop of the incident).
func TestDatabaseStallDoesNotDropAWorker(t *testing.T) {
	hub, db := openHub(t)
	ws, welcome := newStormWorker(t, hub, db).connect()
	defer func() { _ = ws.Close() }()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			var f workerwire.Frame
			if ws.ReadJSON(&f) != nil {
				return
			}
		}
	}()
	tx, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	_ = tx.Rollback()
	select {
	case <-closed:
		t.Fatal("the hub dropped the worker during a database stall")
	case <-time.After(1500 * time.Millisecond):
	}
	if !hub.Connected(welcome.WorkerID) {
		t.Fatal("worker not connected after the stall")
	}
}
