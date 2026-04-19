package claudecode

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
)

type Adapter struct{}

func (Adapter) ID() string { return "claude-code" }

func (Adapter) Build(plan *launch.Plan, workdir string) (*exec.Cmd, error) {
	if plan.Command == "" {
		return nil, fmt.Errorf("provider command empty")
	}
	cmd := exec.Command(plan.Command, plan.Args...)
	cmd.Dir = workdir
	cmd.Env = provider.BuildEnv(plan.EnvMode, plan.EnvPassthrough, plan.EnvRedact, plan.Env, os.Environ())
	return cmd, nil
}
