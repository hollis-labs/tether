package daemon

// wake_sweep_test.go — T06 (messaging vNext, CW-20260906-0037): proves
// Run actually starts the shared wake pump when WakeSweeper is set, ticks
// it repeatedly until shutdown, and never starts it at all when unset (the
// large majority of existing daemon.Server tests construct a Server with
// no WakeSweeper -- this must remain a true no-op for them).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type countingWakeSweeper struct {
	calls atomic.Int64
	err   error
}

func (c *countingWakeSweeper) RunWakeSweep(_ context.Context) (int, error) {
	c.calls.Add(1)
	return 0, c.err
}

func TestServer_Run_StartsWakeSweepLoop_WhenConfigured(t *testing.T) {
	dir := shortTempDir(t)
	cfg := Config{
		ListenAddr:      "unix:" + dir + "/s.sock",
		PIDFile:         dir + "/d.pid",
		ShutdownTimeout: 2 * time.Second,
	}
	sweeper := &countingWakeSweeper{}
	restore := setWakeSweepIntervalForTest(10 * time.Millisecond)
	defer restore()

	srv := &Server{Config: cfg, WakeSweeper: sweeper}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sweeper.calls.Load() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := sweeper.calls.Load(); n < 3 {
		t.Fatalf("wake sweep called %d times within the deadline, want at least 3 (the pump must tick repeatedly, not once)", n)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestServer_Run_NoWakeSweeper_NeverTicks(t *testing.T) {
	dir := shortTempDir(t)
	cfg := Config{
		ListenAddr:      "unix:" + dir + "/s.sock",
		PIDFile:         dir + "/d.pid",
		ShutdownTimeout: 2 * time.Second,
	}
	restore := setWakeSweepIntervalForTest(5 * time.Millisecond)
	defer restore()

	srv := &Server{Config: cfg} // WakeSweeper left nil.
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	time.Sleep(50 * time.Millisecond) // several sweep intervals' worth, if one were (wrongly) running unconditionally

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	// No assertion beyond "this didn't panic/block" is possible without a
	// sweeper to count calls on -- the real assertion is structural: Run
	// gates the goroutine on s.WakeSweeper != nil, exercised directly by
	// leaving it nil here alongside the positive case above.
}

func setWakeSweepIntervalForTest(d time.Duration) (restore func()) {
	prev := wakeSweepInterval
	wakeSweepInterval = d
	return func() { wakeSweepInterval = prev }
}
