package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/events"
)

type recorderPublisher struct {
	mu  sync.Mutex
	evs []events.Event
}

func (r *recorderPublisher) Publish(_ context.Context, e events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, e)
	return nil
}

func (r *recorderPublisher) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, len(r.evs))
	copy(out, r.evs)
	return out
}

func (r *recorderPublisher) findKind(k string) *events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.evs {
		if r.evs[i].Kind == k {
			return &r.evs[i]
		}
	}
	return nil
}

func TestServer_Run_EmitsDaemonLifecycleEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	cfg := Config{
		ListenAddr:      "unix:" + filepath.Join(dir, "s.sock"),
		PIDFile:         filepath.Join(dir, "d.pid"),
		ShutdownTimeout: 2 * time.Second,
	}

	pub := &recorderPublisher{}
	srv := &Server{
		Config:    cfg,
		Publisher: pub,
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	// Wait for socket to appear as a proxy for "daemon is up".
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "s.sock")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	started := pub.findKind(events.KindDaemonStarted)
	if started == nil {
		t.Fatal("no daemon.started event published")
	}
	if started.Scope != events.ScopeDaemon {
		t.Errorf("started.Scope = %q, want daemon", started.Scope)
	}
	if started.SessionID != "" {
		t.Errorf("started.SessionID = %q, want empty", started.SessionID)
	}
	var startedPayload struct {
		Version  string `json:"version"`
		PID      int    `json:"pid"`
		Listener string `json:"listener"`
	}
	if err := json.Unmarshal([]byte(started.PayloadJSON), &startedPayload); err != nil {
		t.Fatalf("started payload: %v (raw=%q)", err, started.PayloadJSON)
	}
	if startedPayload.PID != os.Getpid() {
		t.Errorf("started.pid = %d, want %d", startedPayload.PID, os.Getpid())
	}
	if startedPayload.Listener != cfg.ListenAddr {
		t.Errorf("started.listener = %q, want %q", startedPayload.Listener, cfg.ListenAddr)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	shutStart := pub.findKind(events.KindDaemonShutdownStarted)
	if shutStart == nil {
		t.Fatal("no daemon.shutdown_started event")
	}
	shutDone := pub.findKind(events.KindDaemonShutdownCompleted)
	if shutDone == nil {
		t.Fatal("no daemon.shutdown_completed event")
	}

	// Ordering: started → shutdown_started → shutdown_completed in snapshot.
	snap := pub.snapshot()
	idx := func(kind string) int {
		for i, e := range snap {
			if e.Kind == kind {
				return i
			}
		}
		return -1
	}
	is := idx(events.KindDaemonStarted)
	iss := idx(events.KindDaemonShutdownStarted)
	isc := idx(events.KindDaemonShutdownCompleted)
	if is >= iss || iss >= isc {
		t.Errorf("event order wrong: started=%d shutdown_started=%d shutdown_completed=%d", is, iss, isc)
	}
}

func TestServer_Run_NoPublisher_NoCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket test requires unix")
	}
	dir := shortTempDir(t)
	cfg := Config{
		ListenAddr:      "unix:" + filepath.Join(dir, "s.sock"),
		PIDFile:         filepath.Join(dir, "d.pid"),
		ShutdownTimeout: 2 * time.Second,
	}
	srv := &Server{Config: cfg} // no Publisher

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "s.sock")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
}
