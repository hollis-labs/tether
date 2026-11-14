package workerhub_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type stallRig struct {
	hub    *workerhub.Hub
	ws     *websocket.Conn
	worker string
	// seen receives every frame the hub sent that is not a command (replay requests, pings).
	seen chan workerwire.Frame
	// closed is closed when the hub drops the connection.
	closed chan struct{}
}

// newStallRig connects one fake worker that answers every command frame with an ack.
func newStallRig(t *testing.T, hub *workerhub.Hub, db *store.Store, answer func(workerwire.Frame) bool) *stallRig {
	t.Helper()
	ids := identity.NewStore(db.DB())
	expires := time.Now().Add(time.Hour)
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://device/stall-worker", Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&daemon.Server{Identity: ids, Workers: hub, Config: daemon.Config{RemoteListener: daemon.RemoteListenerConfig{AllowedHosts: []string{"worker.test"}}}}).RemoteHandler("worker.test"))
	t.Cleanup(server.Close)
	header := http.Header{}
	header.Set("Host", "worker.test")
	header.Set("Authorization", "Bearer "+token)
	header.Set("Tether-Protocol", "1")
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+workerwire.ConnectPath, header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if err := ws.WriteJSON(workerwire.Frame{T: "hello", Proto: 1, Worker: &workerwire.Worker{Label: "stall-worker", Platform: "linux", Arch: "amd64"}}); err != nil {
		t.Fatal(err)
	}
	var welcome workerwire.Frame
	if err := ws.ReadJSON(&welcome); err != nil || welcome.T != "welcome" {
		t.Fatalf("welcome: %+v %v", welcome, err)
	}
	r := &stallRig{hub: hub, ws: ws, worker: welcome.WorkerID, seen: make(chan workerwire.Frame, 64), closed: make(chan struct{})}
	go func() {
		defer close(r.closed)
		for {
			var f workerwire.Frame
			if ws.ReadJSON(&f) != nil {
				return
			}
			if f.CmdID == "" {
				select {
				case r.seen <- f:
				default:
				}
				continue
			}
			if answer == nil || answer(f) {
				_ = ws.WriteJSON(workerwire.Frame{T: "ack", CmdID: f.CmdID, Session: f.Session})
			}
		}
	}()
	return r
}

func openHub(t *testing.T) (*workerhub.Hub, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := workerhub.New(db.DB())
	t.Cleanup(hub.Close)
	return hub, db
}

// CW-20261114-0104: the hub routed a worker's command responses on the same goroutine that ingested its
// session frames (fsync, SQLite, turn-output persistence). Under load that ingest stalled, every command
// to the worker (a turn, a mail wake) timed out at 30s while the worker stayed "connected", and only a hub
// restart cleared it. Here ingest of one output frame blocks outright: a command sent after it must still
// get its answer.
func TestCommandsAreAnsweredWhileIngestIsStalled(t *testing.T) {
	hub, db := openHub(t)
	release := make(chan struct{})
	var ingested atomic.Int32
	hub.Event = func(_ string, f workerwire.Frame) error {
		if f.T == "output" {
			<-release // a stalled fsync or database
		}
		ingested.Add(1)
		return nil
	}
	r := newStallRig(t, hub, db, nil)
	defer close(release)
	for i := int64(1); i <= 3; i++ {
		if err := r.ws.WriteJSON(workerwire.Frame{T: "output", Session: "s1", Seq: i, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := r.hub.Send(ctx, r.worker, workerwire.Frame{T: "turn", CmdID: "turn-1", Session: "s2"}); err != nil {
		t.Fatalf("a turn to a worker whose ingest is stalled: %v after %s", err, time.Since(start))
	}
}

// A burst larger than the ingest queue while ingest is stalled is dropped, never queued without bound and
// never by blocking the reader; once ingest drains, the hub asks the worker to replay that session from
// the stored cursor, so nothing is lost.
func TestIngestOverflowIsReplayedFromTheCursor(t *testing.T) {
	hub, db := openHub(t)
	release := make(chan struct{})
	var stalled atomic.Bool
	hub.Event = func(_ string, f workerwire.Frame) error {
		if stalled.CompareAndSwap(false, true) {
			<-release
		}
		return nil
	}
	r := newStallRig(t, hub, db, nil)
	for i := int64(1); i <= 4200; i++ {
		if err := r.ws.WriteJSON(workerwire.Frame{T: "output", Session: "busy", Seq: i, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := r.hub.Send(ctx, r.worker, workerwire.Frame{T: "turn", CmdID: "turn-during-burst", Session: "other"}); err != nil {
		t.Fatalf("command during an ingest overflow: %v", err)
	}
	list, _ := hub.List(context.Background())
	if len(list) != 1 || list[0].DroppedFrames == 0 || list[0].IngestBacklog == 0 {
		t.Fatalf("a full queue must drop frames and show the backlog: %+v", list)
	}
	close(release)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-r.seen:
			if f.T == "replay" && f.Session == "busy" {
				return
			}
		case <-deadline:
			t.Fatal("the dropped session was never replayed")
		}
	}
}

// A worker that keeps not answering is reported degraded after DegradedAfter timeouts in a row and
// disconnected after ReconnectAfter, so it reconnects and resumes: the recovery a hub restart gave by hand.
func TestUnansweredCommandsReportThenReconnect(t *testing.T) {
	hub, db := openHub(t)
	var reports []workerhub.DeliveryDegraded
	reported := make(chan struct{}, 8)
	hub.Degraded = func(_ string, d workerhub.DeliveryDegraded) {
		reports = append(reports, d)
		reported <- struct{}{}
	}
	r := newStallRig(t, hub, db, func(workerwire.Frame) bool { return false })
	for i := 1; i <= workerhub.ReconnectAfter; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err := hub.Send(ctx, r.worker, workerwire.Frame{T: "turn", CmdID: fmt.Sprint("turn-", i), Session: "s"})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send %d: %v", i, err)
		}
		if i == workerhub.DegradedAfter-1 {
			if list, _ := hub.List(context.Background()); len(list) != 1 || list[0].DeliveryTimeouts != i {
				t.Fatalf("timeouts not shown: %+v", list)
			}
		}
	}
	for range 2 {
		select {
		case <-reported:
		case <-time.After(time.Second):
			t.Fatalf("reports = %+v", reports)
		}
	}
	if len(reports) != 2 || reports[0].Action != "reported" || reports[0].ConsecutiveTimeouts != workerhub.DegradedAfter ||
		reports[1].Action != "reconnect" || reports[1].ConsecutiveTimeouts != workerhub.ReconnectAfter {
		t.Fatalf("reports = %+v", reports)
	}
	select {
	case <-r.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the hub kept the connection of a worker that stopped answering")
	}
}

// One command a worker never answers costs only that command: the next one is answered, and an answer
// ends the run of timeouts.
func TestOneStuckCommandDoesNotBlockOthers(t *testing.T) {
	hub, db := openHub(t)
	r := newStallRig(t, hub, db, func(f workerwire.Frame) bool { return f.CmdID != "stuck" })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := hub.Send(ctx, r.worker, workerwire.Frame{T: "turn", CmdID: "stuck", Session: "s"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stuck: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := hub.Send(ctx, r.worker, workerwire.Frame{T: "turn", CmdID: "next", Session: "s"}); err != nil {
		t.Fatalf("next command: %v", err)
	}
	if list, _ := hub.List(context.Background()); len(list) != 1 || list[0].DeliveryTimeouts != 0 {
		t.Fatalf("an answer must end the run of timeouts: %+v", list)
	}
}
