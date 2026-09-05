package otel

import (
	"bytes"
	"io"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// restoreLogging snapshots the process-global logging state that
// configureLogging mutates, so these tests do not leak into each other or into
// the rest of the package's tests.
func restoreLogging(t *testing.T) {
	t.Helper()

	prevSlog := slog.Default()
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
}

// TestConfigureLoggingDoesNotDeadlockLogPrintf is the regression test for the
// hang that took down `mux mcp`: wrapping slog's built-in default handler made
// log.Printf reenter log.std's mutex and block forever.
func TestConfigureLoggingDoesNotDeadlockLogPrintf(t *testing.T) {
	restoreLogging(t)

	configureLogging()

	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Printf("WARN: resolving deprecated PTY runtime for provider %q", "claude-pty")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("log.Printf did not return after configureLogging: the slog/log default-handler cycle is back")
	}
}

// TestConfigureLoggingWritesToStderrNotStdout pins the destination: the stdio
// MCP transport owns stdout, so a stray log line there corrupts the protocol
// stream.
func TestConfigureLoggingWritesToStderrNotStdout(t *testing.T) {
	restoreLogging(t)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	configureLogging()

	log.Printf("hello from the log package")
	slog.Info("hello from slog")

	got := buf.String()
	if !strings.Contains(got, "hello from the log package") {
		t.Fatalf("log.Printf output missing from handler sink: %q", got)
	}
	if !strings.Contains(got, "hello from slog") {
		t.Fatalf("slog output missing from handler sink: %q", got)
	}
}

func TestBaseHandlerReplacesBuiltinDefault(t *testing.T) {
	restoreLogging(t)

	slog.SetDefault(slog.New(builtinDefaultHandler))

	if got := baseHandler(); got == builtinDefaultHandler {
		t.Fatal("baseHandler returned slog's built-in default handler; wrapping it deadlocks log.Printf")
	}
}

func TestBaseHandlerPreservesCustomDefault(t *testing.T) {
	restoreLogging(t)

	custom := slog.NewJSONHandler(io.Discard, nil)
	slog.SetDefault(slog.New(custom))

	if got := baseHandler(); got != custom {
		t.Fatalf("baseHandler = %T, want the caller's own handler %T", got, custom)
	}
}
