package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type countingEventRetention struct{ calls atomic.Int64 }

func (c *countingEventRetention) RunEventRetention(context.Context) (int64, error) {
	c.calls.Add(1)
	return 0, nil
}

// Run ticks the events retention sweep through the shared periodic loop when
// it is configured (CW-20260930-0008).
func TestServer_Run_StartsEventRetentionLoop_WhenConfigured(t *testing.T) {
	dir := shortTempDir(t)
	cfg := Config{
		ListenAddr:      "unix:" + dir + "/s.sock",
		PIDFile:         dir + "/d.pid",
		ShutdownTimeout: 2 * time.Second,
	}
	prev := eventRetentionInterval
	eventRetentionInterval = 10 * time.Millisecond
	defer func() { eventRetentionInterval = prev }()

	retention := &countingEventRetention{}
	srv := &Server{Config: cfg, EventRetention: retention}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && retention.calls.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := retention.calls.Load(); n < 2 {
		t.Fatalf("events retention ran %d times; want it to tick repeatedly", n)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
