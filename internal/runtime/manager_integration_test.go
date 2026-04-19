package runtime

import (
	"context"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/chrispian/agent-mux/internal/launch"
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
			LaunchID:   "integ",
			ProjectID:  "p",
			AgentID:    "a",
			ProviderID: "cli",
			Command:    "/bin/echo",
			Args:       []string{"hello-from-mux"},
			RepoRoot:   tmp,
			EnvMode:    "merge",
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
