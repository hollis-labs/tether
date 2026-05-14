package app

import (
	"os/exec"
	"testing"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	"github.com/hollis-labs/go-agent-sessions/compliance"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestClaudeCodeRuntimeCompliance(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	factory, err := runtimeFactoryForProvider(config.Provider{
		ID:          "claude-code",
		Provider:    "claude",
		RuntimeKind: config.RuntimeKindStreamingStdio,
	})
	if err != nil {
		t.Fatalf("runtimeFactoryForProvider: %v", err)
	}
	rt, err := factory(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("factory: %v", err)
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
