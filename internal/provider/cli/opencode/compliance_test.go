package opencode

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	"github.com/hollis-labs/go-agent-sessions/compliance"

	"github.com/hollis-labs/tether/internal/launch"
)

// TestCompliance runs the shared go-agent-sessions compliance suite against
// the opencode Runtime using a sh-backed plan so no real opencode CLI is
// needed.
func TestCompliance(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	rt, err := New(&launch.Plan{Command: shPath, Args: []string{"-c", "exit 0"}})
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
		BinarySkip: true, // skip cap-tests requiring real opencode binary
	})
}
