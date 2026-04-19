package runtime

import (
	"context"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider/api/stub"
	"github.com/chrispian/agent-mux/internal/provider/cli/claudecode"
	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/workspace"
)

// TestManager_RealPTYLifecycle exercises the real claudecode.Adapter
// (session.Start / creack/pty) with a trivial command to prove end-to-end
// state transitions on a real Session. Skipped on non-unix platforms and
// when /bin/echo is missing.
func TestManager_RealPTYLifecycle(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("PTY path requires unix")
	}
	if _, err := exec.LookPath("/bin/echo"); err != nil {
		t.Skipf("/bin/echo missing: %v", err)
	}

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "session.log")

	sink := &fakeSink{}
	m := NewManager(sink)

	req := StartRequest{
		ID: "sess-real",
		Plan: &launch.Plan{
			LaunchID:       "integ",
			ProjectID:      "p",
			LogicalAgentID: "a",
			ProviderID:     "cli",
			Command:        "/bin/echo",
			Args:           []string{"hello-from-mux"},
			RepoRoot:       tmp,
			EnvMode:        "merge",
		},
		Workspace: &workspace.Session{
			ID:      "sess-real",
			Root:    tmp,
			LogPath: logPath,
		},
		Runtime: claudecode.Adapter{},
	}

	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	code, err := m.WaitSession(context.Background(), "sess-real")
	if err != nil {
		t.Fatalf("WaitSession: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if !sink.has("sess-real", string(session.StateLaunching)) {
		t.Errorf("missing launching transition: %+v", sink.snapshot())
	}
	if !sink.has("sess-real", string(session.StateRunning)) {
		t.Errorf("missing running transition: %+v", sink.snapshot())
	}
	if !sink.has("sess-real", string(session.StateCompleted)) {
		t.Errorf("missing completed transition: %+v", sink.snapshot())
	}
}

// TestManager_APIStubLifecycle is the end-to-end gate proving the non-CLI
// runtime contract: Manager.Start → stub.Runtime.Start → provider.Session
// flows through launching→running→killed, SendInput delivers bytes that the
// stub echoes back through Attach, and Stop transitions cleanly. No process
// is spawned; if this test ever requires GOOS branching, the contract has
// regressed.
func TestManager_APIStubLifecycle(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "session.log")

	sink := &fakeSink{}
	m := NewManager(sink)

	req := StartRequest{
		ID: "sess-stub",
		Plan: &launch.Plan{
			LaunchID:       "api-stub-launch",
			ProjectID:      "demo",
			LogicalAgentID: "demo-agent",
			ProviderID:     "api-stub",
			EnvMode:        "merge",
		},
		Workspace: &workspace.Session{
			ID:      "sess-stub",
			Root:    tmp,
			LogPath: logPath,
		},
		Runtime: stub.Runtime{},
	}

	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Session must report running immediately; no process means PID=0.
	info, ok := m.Get("sess-stub")
	if !ok {
		t.Fatal("expected session to be registered")
	}
	if info.State != session.StateRunning {
		t.Errorf("State = %q, want running", info.State)
	}
	if info.PID != 0 {
		t.Errorf("api-stub session should report PID=0, got %d", info.PID)
	}

	// Attach before sending input; the broker replay-plus-live contract means
	// a post-input attach would also see the echo, but the attach-before-input
	// path is the one CLI consumers will exercise.
	var buf threadsafeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	attachDone := make(chan error, 1)
	go func() { attachDone <- m.Attach(ctx, "sess-stub", &buf) }()

	// Small wait so the subscription is registered before we emit.
	time.Sleep(30 * time.Millisecond)

	if err := m.SendInput("sess-stub", []byte("hello")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}

	deadline := time.After(1 * time.Second)
	for {
		if strings.Contains(buf.String(), "echo: hello\n") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("attach buf = %q, want to contain %q", buf.String(), "echo: hello\n")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Tear down via Manager.Stop — must record StateKilled and return 0.
	if err := m.Stop(context.Background(), "sess-stub"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if !sink.has("sess-stub", string(session.StateKilled)) {
		t.Errorf("missing killed transition: %+v", sink.snapshot())
	}
	if _, ok := m.Get("sess-stub"); ok {
		t.Error("session should be unregistered after stop")
	}

	cancel()
	<-attachDone
}
