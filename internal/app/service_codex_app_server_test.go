package app

import (
	"os/exec"
	"testing"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	"github.com/hollis-labs/go-agent-sessions/compliance"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestCodexAppServerRuntimeCompliance(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	rt, err := newCodexAppServerRuntime(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("newCodexAppServerRuntime: %v", err)
	}
	compliance.Run(t, compliance.Harness{
		Runtime: rt,
		NewStartOptions: func(t *testing.T) agentsessions.StartOptions {
			dir := t.TempDir()
			return agentsessions.StartOptions{
				Workdir:      dir,
				WorkspaceDir: dir,
			}
		},
		SkipEventFanout: true,
	})
}
