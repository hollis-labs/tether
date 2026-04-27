package claudecode

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	"github.com/hollis-labs/go-agent-sessions/compliance"

	"github.com/chrispian/agent-mux/internal/launch"
)

// TestCompliance runs the shared go-agent-sessions compliance suite against
// the claudecode (PTY) Runtime. A long-running sh stand-in keeps the PTY
// alive across the lifecycle tests; the real claude CLI isn't needed.
func TestCompliance(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	// `sh -c "while true; do sleep 1; done"` keeps the PTY child alive
	// indefinitely so Stop-driven lifecycle tests have something to kill.
	rt, err := New(&launch.Plan{
		Command: shPath,
		Args:    []string{"-c", "while true; do sleep 1; done"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	compliance.Run(t, compliance.Harness{
		Runtime: rt,
		NewStartOptions: func(t *testing.T) agentsessions.StartOptions {
			dir := t.TempDir()
			return agentsessions.StartOptions{
				Workdir: dir,
				LogPath: filepath.Join(dir, "session.log"),
			}
		},
	})
}
