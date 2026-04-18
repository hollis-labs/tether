package claudecode

import (
	"fmt"
	"os/exec"

	"github.com/chrispian/agent-mux/internal/launch"
)

type Adapter struct{}

func (Adapter) ID() string { return "claude-code" }

func (Adapter) Build(plan *launch.Plan, workdir string) (*exec.Cmd, error) {
	if plan.Command == "" {
		return nil, fmt.Errorf("provider command empty")
	}
	cmd := exec.Command(plan.Command, plan.Args...)
	cmd.Dir = workdir
	cmd.Env = flattenEnv(plan.Env)
	return cmd, nil
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
