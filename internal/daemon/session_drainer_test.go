package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type countingDrainer struct{ calls atomic.Int64 }

func (d *countingDrainer) DrainSessions(ctx context.Context) error {
	d.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return context.Canceled // Run must bound the drain with ShutdownTimeout
	}
	return nil
}

// A graceful shutdown drains sessions through the SessionDrainer, so the
// app can record why they ended (CW-20260912-0086).
func TestServer_Run_DrainsThroughSessionDrainer(t *testing.T) {
	dir := shortTempDir(t)
	drainer := &countingDrainer{}
	srv := &Server{Config: Config{
		ListenAddr:      "unix:" + dir + "/s.sock",
		PIDFile:         dir + "/d.pid",
		ShutdownTimeout: 2 * time.Second,
	}, SessionDrainer: drainer}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	if n := drainer.calls.Load(); n != 1 {
		t.Fatalf("DrainSessions called %d times, want 1", n)
	}
}
