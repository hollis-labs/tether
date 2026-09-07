package e2e

// split_brain_test.go — T11 durability review evidence (CW-20260906-0042):
// a live-reproduced bug where a second `mux daemon run` invocation
// against the same state root could (a) mutate the live daemon's
// database (ReconcileStaleState + registry bootstrap) before its own
// PID-file check ever ran, and (b), if the PID file was missing or
// stale-looking for any reason, actually steal the live daemon's socket
// and run fully split-brain against the same state.db. Fixed by an
// early pre-flight liveness check in cmd/mux/daemon.go and by hardening
// internal/daemon/listener_unix.go's removeStaleSocket to refuse
// stealing a socket something is still actually listening on. This test
// reproduces both scenarios against the real built binary.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSplitBrain_SecondDaemonRunAgainstSameStateRootIsRefused(t *testing.T) {
	first := StartFixtureDaemon(t)

	second := exec.Command(muxBinary(t), "daemon", "run", "--catalog", first.CatalogDir) //nolint:gosec // G204: our own just-built test binary
	out, err := second.CombinedOutput()
	if err == nil {
		t.Fatalf("second daemon run unexpectedly exited 0 against a live state root; output:\n%s", out)
	}

	// The first daemon must be completely unaffected -- still healthy,
	// still the one actually holding the socket.
	if err := first.Client().Ping(t.Context()); err != nil {
		t.Fatalf("original daemon unresponsive after a rejected double-start attempt: %v", err)
	}
}

func TestSplitBrain_DeletedPIDFileStillRefusesToStealTheSocket(t *testing.T) {
	first := StartFixtureDaemon(t)

	// Simulate an external actor removing the PID file out from under a
	// live daemon (a cleanup script, a disk hiccup) -- the exact scenario
	// the durability review used to defeat the PID-file-only guard.
	pidFile := filepath.Join(first.StateRoot, "run", "muxd.pid")
	if err := os.Remove(pidFile); err != nil {
		t.Fatalf("remove pid file: %v", err)
	}

	second := exec.Command(muxBinary(t), "daemon", "run", "--catalog", first.CatalogDir) //nolint:gosec // G204: our own just-built test binary
	// This second process will hang serving (or fail immediately) rather
	// than exit cleanly if the bug is present, since a stolen socket
	// means it thinks it started successfully. Give it a bounded window,
	// then confirm the ORIGINAL daemon (not this one) is still the one
	// answering, and kill this one regardless of what happened.
	if err := second.Start(); err != nil {
		t.Fatalf("start second daemon run: %v", err)
	}
	defer func() {
		if second.Process != nil {
			_ = second.Process.Kill()
			_, _ = second.Process.Wait()
		}
	}()

	time.Sleep(500 * time.Millisecond)

	if err := first.Client().Ping(t.Context()); err != nil {
		t.Fatalf("original daemon unresponsive after a second invocation raced its (removed) pid file: %v", err)
	}

	// If the bug were present, the second process would have stolen the
	// socket and be running as an independent live daemon right now
	// (Process.Wait would block). With the fix, it should have already
	// exited (refused to steal a live socket) -- confirm that rather than
	// leaving an orphaned process for the deferred Kill to paper over.
	done := make(chan error, 1)
	go func() { done <- second.Wait() }()
	select {
	case waitErr := <-done:
		if waitErr == nil {
			t.Fatal("second daemon run exited 0 -- it should have refused to steal the live socket")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second daemon run is still running -- it stole the socket instead of refusing (split-brain)")
	}
}
