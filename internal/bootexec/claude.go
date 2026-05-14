package bootexec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/launch"
	tetherprovider "github.com/hollis-labs/tether/internal/provider"
)

// Options carries direct-exec-only inputs that are normally supplied by the
// agentsessions StartOptions path.
type Options struct {
	BootDirRoot      string
	APIKeyHelperPath string
	MuxCommand       string
	MuxArgs          []string
	MuxEnv           []string
	ParentEnv        []string
}

// Prepared is the fully materialized direct CLI invocation.
type Prepared struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
	BootDir string
}

// PrepareClaudeTUI materializes the Claude boot-dir layout and returns a
// command line that runs the real Claude TUI in the caller's terminal.
func PrepareClaudeTUI(plan *launch.Plan, opts Options) (*Prepared, error) {
	if plan == nil {
		return nil, fmt.Errorf("launch plan required")
	}
	if plan.ProviderBrand != "claude" {
		return nil, fmt.Errorf("boot-exec currently supports claude launch profiles only (got provider %q)", plan.ProviderBrand)
	}

	adapter := gop.NewClaudeAdapterPTY()
	adapter.ApiKeyHelperPath = opts.APIKeyHelperPath
	spec := adapter.BootDirSpec()
	bootDir, err := materializeBootDir(spec, plan, opts)
	if err != nil {
		return nil, err
	}

	env := tetherprovider.BuildEnv(plan.EnvMode, plan.EnvPassthrough, plan.EnvRedact, plan.Env, opts.ParentEnv)
	env = append(env, substituteTemplates(spec.EnvAmendments, bootDir, plan.RepoRoot)...)

	args := append([]string(nil), plan.Args...)
	args = append(args, adapter.BuildArgs("", "", "")...)
	args = append(args, substituteArgTokens(spec.ProjectDirArg, bootDir, plan.RepoRoot)...)

	command := plan.Command
	if command == "" {
		if detected, ok := adapter.Detect(); ok {
			command = detected
		}
	}
	if command == "" {
		return nil, fmt.Errorf("claude command is empty and could not be detected")
	}

	return &Prepared{
		Command: command,
		Args:    args,
		Dir:     spec.SpawnWorkdir(bootDir, plan.RepoRoot),
		Env:     env,
		BootDir: bootDir,
	}, nil
}

func materializeBootDir(spec gop.BootDirSpec, plan *launch.Plan, opts Options) (string, error) {
	if len(spec.PlantedFiles) == 0 {
		return "", fmt.Errorf("provider has no boot-dir planting spec")
	}
	root := opts.BootDirRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("ensure boot-exec root %s: %w", root, err)
	}
	bootDir, err := os.MkdirTemp(root, "tether-boot-exec-claude-*")
	if err != nil {
		return "", fmt.Errorf("create boot-exec dir: %w", err)
	}

	ctx := gop.PlantContext{
		SystemPrompt: plan.BootPrompt,
		BootContent:  plan.BootPrompt,
		AgentName:    plan.LogicalAgentID,
		ProjectDir:   plan.RepoRoot,
		BootDir:      bootDir,
		MuxCommand:   opts.MuxCommand,
		MuxArgs:      append([]string(nil), opts.MuxArgs...),
		MuxEnv:       append([]string(nil), opts.MuxEnv...),
	}

	for _, pf := range spec.PlantedFiles {
		path := filepath.Join(bootDir, pf.RelPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			_ = os.RemoveAll(bootDir)
			return "", fmt.Errorf("plant %s: mkdir: %w", pf.RelPath, err)
		}
		if pf.Render == nil {
			continue
		}
		content, err := pf.Render(ctx)
		if err != nil {
			_ = os.RemoveAll(bootDir)
			return "", fmt.Errorf("plant %s: render: %w", pf.RelPath, err)
		}
		if err := os.WriteFile(path, []byte(content), plantedFileMode(pf)); err != nil {
			_ = os.RemoveAll(bootDir)
			return "", fmt.Errorf("plant %s: write: %w", pf.RelPath, err)
		}
	}
	return bootDir, nil
}

func plantedFileMode(pf gop.PlantedFile) os.FileMode {
	if pf.Mode != 0 {
		return pf.Mode
	}
	if pf.RelPath == ".mcp.json" || strings.HasSuffix(pf.RelPath, "settings.json") {
		return 0o600
	}
	return 0o644
}

func substituteTemplates(in []string, bootDir, projectDir string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ReplaceAll(s, "{{.BootDir}}", bootDir)
		s = strings.ReplaceAll(s, "{{.ProjectDir}}", projectDir)
		out = append(out, s)
	}
	return out
}

func substituteArgTokens(template, bootDir, projectDir string) []string {
	if template == "" || projectDir == "" {
		return nil
	}
	parts := strings.Fields(template)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ReplaceAll(part, "{{.BootDir}}", bootDir)
		part = strings.ReplaceAll(part, "{{.ProjectDir}}", projectDir)
		out = append(out, part)
	}
	return out
}
