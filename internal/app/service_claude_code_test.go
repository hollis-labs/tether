package app

import (
	"os/exec"
	"testing"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	"github.com/hollis-labs/go-agent-sessions/compliance"

	"github.com/chrispian/agent-mux/internal/launch"
)

func TestClaudeCodeRuntimeCompliance(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	rt, err := newClaudeCodeRuntime(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("newClaudeCodeRuntime: %v", err)
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
