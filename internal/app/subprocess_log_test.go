package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/runner"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

func TestSubprocessLog_TurnAddsBoundedStderrTail(t *testing.T) {
	l, err := openSubprocessLog(filepath.Join(t.TempDir(), "logs", "session.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	err = l.turn(func() error {
		_, _ = l.Stderr().Write([]byte(strings.Repeat("x", 3*subprocessStderrTailBytes)))
		_, _ = l.Stderr().Write([]byte("é\nError: 401 Unauthorized\n"))
		return fmt.Errorf("agentsessions: %w", &runner.ExitError{Code: 1})
	})
	var exit *runner.ExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err = %v, want it to wrap *runner.ExitError{Code: 1}", err)
	}
	msg := err.Error()
	if !strings.HasSuffix(msg, "Error: 401 Unauthorized") {
		t.Fatalf("message does not end with the stderr tail: %q", msg[max(0, len(msg)-80):])
	}
	tail := msg[strings.Index(msg, "):\n")+3:]
	if len(tail) > subprocessStderrTailBytes {
		t.Fatalf("stderr tail is %d bytes, want at most %d", len(tail), subprocessStderrTailBytes)
	}

	// The next turn starts with an empty tail.
	err = l.turn(func() error { return &runner.ExitError{Code: 2} })
	if strings.Contains(err.Error(), "401") {
		t.Fatalf("second turn carried the first turn's stderr: %v", err)
	}
}

func TestSubprocessLog_NonExitErrorsPassThrough(t *testing.T) {
	l, err := openSubprocessLog(filepath.Join(t.TempDir(), "session.log"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Stderr().Write([]byte("noise"))
	busy := agentsessions.ErrTurnInFlight
	if got := l.turn(func() error { return busy }); !errors.Is(got, busy) || got.Error() != busy.Error() {
		t.Fatalf("turn = %v, want %v unchanged", got, busy)
	}
	if got := l.turn(func() error { return nil }); got != nil {
		t.Fatalf("turn = %v, want nil", got)
	}
	_ = l.Close()
	if n, err := l.Write([]byte("after close")); n != len("after close") || err != nil {
		t.Fatalf("Write after Close = %d, %v; must never fail the attach fan-out", n, err)
	}
}

// fakeCodex stands in for `codex exec <prompt> --json`: a prompt containing
// FAIL prints an auth error to stderr and exits 1; anything else prints a
// codex agent message on stdout and a note on stderr.
const fakeCodex = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    *FAIL*)
      echo "Error: unexpected status 401 Unauthorized: Missing bearer or basic authentication" >&2
      exit 1;;
  esac
done
echo "fake-codex: note on stderr" >&2
echo '{"type":"item.completed","item":{"type":"agent_message","text":"FAKE REPLY"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// End to end through LaunchSession and agentkit's adapter runtime: a failed
// turn returns a typed error with the exit status and stderr, and both turns
// land in logs/session.log.
func TestSubprocessSession_TurnErrorAndSessionLog(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte(fakeCodex), 0o755); err != nil { //nolint:gosec // test stand-in must be executable
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const sessID = "sess-subprocess-turns"
	plan := &launch.Plan{
		LaunchID:       "codex-launch",
		ProjectID:      "proj",
		LogicalAgentID: "agent",
		ProviderID:     "codex-cli",
		ProviderBrand:  "codex",
		RuntimeKind:    config.RuntimeKindSubprocess,
		RepoRoot:       t.TempDir(),
		WriteHome:      t.TempDir(),
		WorkspaceMode:  "shared",
		Command:        fake,
	}
	ws, err := workspace.Create(plan.WriteHome, sessID, plan)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	row := store.SessionRow{
		ID: sessID, LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID,
		ProviderID: plan.ProviderID, ProviderKind: "cli", Workspace: ws.Root, State: "created",
	}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatalf("create session: %v", err)
	}
	factory, err := runtimeFactoryForProvider(config.Provider{ID: "codex-cli", Adapter: "codex", RuntimeKind: config.RuntimeKindSubprocess})
	if err != nil {
		t.Fatalf("runtime factory: %v", err)
	}
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
		Store:       db,
		Manager:     agentsessions.NewManager(stateSinkAdapter{db: db}),
		factories:   map[string]RuntimeFactory{"codex-cli": factory},
	}
	if _, err := svc.LaunchSession(sessID); err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), sessID) })

	err = svc.SendTurn(context.Background(), sessID, "please FAIL")
	var exit *runner.ExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("failed turn = %v, want a *runner.ExitError with code 1", err)
	}
	if !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("failed turn error has no stderr tail: %v", err)
	}

	if err := svc.SendTurn(context.Background(), sessID, "hello"); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	logData := waitForLog(t, ws.LogPath, "FAKE REPLY")
	for _, want := range []string{"401 Unauthorized", "fake-codex: note on stderr", "FAKE REPLY"} {
		if !strings.Contains(logData, want) {
			t.Errorf("session.log missing %q:\n%s", want, logData)
		}
	}
}

func waitForLog(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(path) //nolint:gosec // test-owned workspace path
		if strings.Contains(string(data), want) || time.Now().After(deadline) {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
