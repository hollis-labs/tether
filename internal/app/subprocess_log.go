package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-runner/runner"
)

// subprocessStderrTailBytes bounds the stderr a failed turn's error carries.
// The whole stream is in session.log; the error only needs enough to say why.
const subprocessStderrTailBytes = 2048

// subprocessLog keeps a subprocess-runtime session's output in its
// logs/session.log, the file `mux sessions tail` reads (CW-20261001-0033).
//
// The streaming-stdio, jsonrpc-stdio and PTY runtimes write that file
// themselves; agentkit's adapter runtime, which runs one process per turn
// (codex exec, claude -p, opencode run, agy), writes nothing, and without a
// StartOptions.Stderr the process's stderr is discarded. A failed codex turn
// was a bare "runner: process exited 1" and a successful one left no trace
// outside codex's own rollout files.
//
// The log takes two streams: the rendered turn output agentkit tees to
// StartOptions.Fanout (reply text, [tool_use:…], [error] …, [turn_done]) and
// the process's raw stderr. It also keeps the current turn's stderr tail so
// a failed turn can say why.
type subprocessLog struct {
	// turnMu holds one turn's reset-send-read sequence together, so a turn
	// queued behind another cannot clear the tail the first one is reading.
	turnMu sync.Mutex

	mu   sync.Mutex
	f    *os.File
	tail []byte

	// telemetry, when set, brackets each turn under turnMu, so a failed
	// turn's exit status reaches the bus as session.turn_failed and a turn
	// whose provider sent no end-of-turn event still publishes its output
	// (CW-20261001-0058). Set once at launch, before any turn.
	telemetry *turnTelemetry
}

func openSubprocessLog(path string) (*subprocessLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: workspace-managed log path
	if err != nil {
		return nil, err
	}
	return &subprocessLog{f: f}, nil
}

// Write logs rendered turn output. It never reports an error: the Manager
// combines this writer with the attach broker in an io.MultiWriter, and a
// failed log write must not cut off attach subscribers.
func (l *subprocessLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_, _ = l.f.Write(p)
	}
	return len(p), nil
}

// Stderr is the writer for each turn process's stderr: logged, with the
// last subprocessStderrTailBytes kept for the turn's error.
func (l *subprocessLog) Stderr() io.Writer { return subprocessStderr{l} }

type subprocessStderr struct{ l *subprocessLog }

func (w subprocessStderr) Write(p []byte) (int, error) {
	l := w.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_, _ = l.f.Write(p)
	}
	l.tail = append(l.tail, p...)
	if over := len(l.tail) - subprocessStderrTailBytes; over > 0 {
		l.tail = append(l.tail[:0], l.tail[over:]...)
	}
	return len(p), nil
}

// turn runs send as one turn and, when the turn's process exited non-zero,
// adds that turn's stderr tail to the error. The tail is the process's own
// stderr and nothing else — no argv, no env. The returned error is also what
// the turn's telemetry reports.
func (l *subprocessLog) turn(send func() error) (err error) {
	l.turnMu.Lock()
	defer l.turnMu.Unlock()
	if l.telemetry != nil {
		l.telemetry.beginTurn()
		defer func() { l.telemetry.endSubprocessTurn(err) }()
	}

	l.mu.Lock()
	l.tail = l.tail[:0]
	l.mu.Unlock()

	err = send()
	var exit *runner.ExitError
	if err == nil || !errors.As(err, &exit) {
		return err
	}
	l.mu.Lock()
	// The cut can land inside a multi-byte rune; drop the fragment.
	tail := strings.TrimSpace(strings.ToValidUTF8(string(l.tail), ""))
	l.mu.Unlock()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w\nstderr (last %d bytes):\n%s", err, len(tail), tail)
}

func (l *subprocessLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// isSubprocessRuntime reports whether rt is agentkit's adapter runtime —
// one process per turn — rather than a long-lived stdio, PTY or HTTP one.
func isSubprocessRuntime(rt agentsessions.Runtime) bool {
	c := rt.Caps()
	return rt.Kind() == "cli" && !c.PTY && !c.StreamingStdio && !c.JsonRpcStdio && !c.ServeHTTP
}
