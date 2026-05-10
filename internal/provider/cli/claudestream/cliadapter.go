package claudestream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	llmtypes "github.com/hollis-labs/go-llm-types"
	gop "github.com/hollis-labs/go-providers/provider"
	events "github.com/hollis-labs/go-providers/provider/events"

	"github.com/chrispian/agent-mux/internal/launch"
)

// New constructs an agentsessions.Runtime that drives the claude CLI's
// `--print --output-format stream-json` mode through go-providers'
// ClaudeAdapter. Each Session's per-turn subprocess is spawned by
// go-runner under the resolved sandbox profile; per-line stream-json
// events are parsed by the upstream adapter (no custom mux parser).
//
// plan.Command and plan.Args are honored via the PlanScopedAdapter
// wrapper — the catalog declares the binary path + any prefix args
// (e.g. wrapper scripts), and the wrapper makes them stick across
// Detect()/BuildArgs without modifying go-providers' adapter.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	return NewWithAdapter(plan, gop.NewClaudeAdapter(), "claude-stream", agentsessions.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	})
}

// NewWithAdapter is the generic constructor: builds an agentsessions.Runtime
// from any go-providers CLIAdapter, threading plan.Command/Args through a
// plan-scoped wrapper. Used by claudestream.New and by app composition for
// catalog-driven cli-goprovider entries (codex, claude) which
// follow the same per-turn subprocess + stream-JSON pattern as claude.
func NewWithAdapter(plan *launch.Plan, adapter gop.CLIAdapter, providerID string, caps agentsessions.Capabilities) (agentsessions.Runtime, error) {
	return &runtime{
		plan:       plan,
		providerID: providerID,
		caps:       caps,
		adapter: &PlanScopedAdapter{
			Inner:    adapter,
			Binary:   plan.Command,
			BaseArgs: append([]string(nil), plan.Args...),
		},
	}, nil
}

type runtime struct {
	plan       *launch.Plan
	providerID string
	caps       agentsessions.Capabilities
	adapter    *PlanScopedAdapter
}

func (r *runtime) ID() string   { return r.providerID }
func (r *runtime) Kind() string { return "cli" }
func (r *runtime) Caps() agentsessions.Capabilities {
	return r.caps
}

func (r *runtime) Prepare(_ context.Context) error {
	if !r.caps.BinaryRequired {
		return nil
	}
	if _, ok := r.adapter.Detect(); !ok {
		return fmt.Errorf("agentsessions: adapter %q binary not found", r.adapter.Name())
	}
	return nil
}

func (r *runtime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	sessionAdapter := r.adapter.Clone()
	innerAdapter := sessionAdapter.Inner
	layout, err := plantBootDir(r.providerID, innerAdapter, opts.BootPrompt, r.plan.RepoRoot, opts.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	if layout != nil {
		if claude, ok := innerAdapter.(*gop.ClaudeAdapter); ok && claude.Bare {
			inj := claude.BareInjectionPaths(layout.BootDir, r.plan.RepoRoot)
			claude.MCPConfigPath = inj.MCPConfigPath
			claude.AppendSystemPromptFile = inj.AppendSystemPromptFile
			claude.SettingsPath = inj.SettingsPath
			claude.ProjectDir = inj.ProjectDir
		}
	}

	buildArgs := func(prompt, sessionID string) []string {
		args := sessionAdapter.BuildArgs(prompt, "", sessionID)
		if layout != nil && len(layout.ProjectDirArg) > 0 {
			skipProjectDirArg := false
			if claude, ok := innerAdapter.(*gop.ClaudeAdapter); ok && claude.Bare {
				skipProjectDirArg = true
			}
			if !skipProjectDirArg {
				args = append(args, layout.ProjectDirArg...)
			}
		}
		return args
	}

	innerRuntime, err := agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{
		ID:        r.providerID,
		Kind:      "cli",
		Adapter:   sessionAdapter,
		Caps:      r.caps,
		BuildArgs: buildArgs,
	})
	if err != nil {
		if layout != nil {
			_ = os.RemoveAll(layout.BootDir)
		}
		return nil, err
	}

	innerOpts := opts
	innerOpts.BootPrompt = ""
	innerOpts.BootMode = ""
	if layout != nil {
		innerOpts.Workdir = layout.SpawnCwd
		innerOpts.Env = append(append([]string(nil), opts.Env...), layout.EnvAmendments...)
	}

	sess, err := innerRuntime.Start(ctx, innerOpts)
	if err != nil {
		if layout != nil {
			_ = os.RemoveAll(layout.BootDir)
		}
		return nil, err
	}
	if layout == nil {
		return sess, nil
	}
	return &bootDirSession{Session: sess, bootDir: layout.BootDir}, nil
}

type bootDirLayout struct {
	BootDir       string
	EnvAmendments []string
	SpawnCwd      string
	ProjectDirArg []string
}

func plantBootDir(providerID string, adapter gop.CLIAdapter, bootPrompt, projectDir, workspaceDir string) (*bootDirLayout, error) {
	bp, ok := adapter.(gop.BootDirProvider)
	if !ok {
		return nil, nil
	}
	spec := bp.BootDirSpec()
	if len(spec.PlantedFiles) == 0 {
		return nil, nil
	}
	bootRoot, err := bootDirRoot(workspaceDir)
	if err != nil {
		return nil, err
	}
	bootDir, err := os.MkdirTemp(bootRoot, fmt.Sprintf("agent-mux-boot-%s-*", sanitizeID(providerID)))
	if err != nil {
		return nil, fmt.Errorf("create boot dir: %w", err)
	}
	plantCtx := gop.PlantContext{
		SystemPrompt: bootPrompt,
		BootContent:  bootPrompt,
		ProjectDir:   projectDir,
		BootDir:      bootDir,
	}
	for _, pf := range spec.PlantedFiles {
		path := filepath.Join(bootDir, pf.RelPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			_ = os.RemoveAll(bootDir)
			return nil, fmt.Errorf("plant %s: mkdir: %w", pf.RelPath, err)
		}
		if pf.Render == nil {
			continue
		}
		content, err := pf.Render(plantCtx)
		if err != nil {
			_ = os.RemoveAll(bootDir)
			return nil, fmt.Errorf("plant %s: render: %w", pf.RelPath, err)
		}
		mode := os.FileMode(0o644)
		if pf.RelPath == ".mcp.json" || strings.HasSuffix(pf.RelPath, "settings.json") {
			mode = 0o600
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			_ = os.RemoveAll(bootDir)
			return nil, fmt.Errorf("plant %s: write: %w", pf.RelPath, err)
		}
	}
	return &bootDirLayout{
		BootDir:       bootDir,
		EnvAmendments: substituteTemplates(spec.EnvAmendments, bootDir, projectDir),
		SpawnCwd:      spec.SpawnWorkdir(bootDir, projectDir),
		ProjectDirArg: substituteArgTokens(spec.ProjectDirArg, bootDir, projectDir),
	}, nil
}

func bootDirRoot(workspaceDir string) (string, error) {
	if workspaceDir == "" {
		return "", nil
	}
	root := filepath.Join(workspaceDir, "boot")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("create boot root: %w", err)
	}
	return root, nil
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

func sanitizeID(s string) string {
	if s == "" {
		return "session"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

type bootDirSession struct {
	agentsessions.Session
	bootDir string
}

func (s *bootDirSession) Wait() (int, error) {
	code, err := s.Session.Wait()
	_ = os.RemoveAll(s.bootDir)
	return code, err
}

func (s *bootDirSession) Stop(ctx context.Context) error {
	err := s.Session.Stop(ctx)
	_ = os.RemoveAll(s.bootDir)
	return err
}

// PlanScopedAdapter wraps a go-providers CLIAdapter so the catalog-
// resolved binary path + prefix args stick. Detect returns the catalog's
// binary unconditionally (catalog is the source of truth — env-var
// fallbacks like CLAUDE_CLI_PATH from go-providers' default would
// confuse multi-launch tenants). BuildArgs prepends Plan.Args before
// the adapter's per-turn argv so wrapper scripts and env-injecting
// prefixes work transparently.
type PlanScopedAdapter struct {
	Inner    gop.CLIAdapter
	Binary   string
	BaseArgs []string
}

func (a *PlanScopedAdapter) Clone() *PlanScopedAdapter {
	return &PlanScopedAdapter{
		Inner:    cloneCLIAdapter(a.Inner),
		Binary:   a.Binary,
		BaseArgs: append([]string(nil), a.BaseArgs...),
	}
}

func (a *PlanScopedAdapter) Name() string { return a.Inner.Name() }

func (a *PlanScopedAdapter) BuildArgs(prompt, systemPrompt, cliSessionID string) []string {
	out := append([]string(nil), a.BaseArgs...)
	return append(out, a.Inner.BuildArgs(prompt, systemPrompt, cliSessionID)...)
}

func (a *PlanScopedAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	return a.Inner.ParseLine(line)
}

func (a *PlanScopedAdapter) Detect() (string, bool) {
	if a.Binary == "" {
		return a.Inner.Detect()
	}
	return a.Binary, true
}

func (a *PlanScopedAdapter) BootDirSpec() gop.BootDirSpec {
	if bp, ok := a.Inner.(gop.BootDirProvider); ok {
		return bp.BootDirSpec()
	}
	return gop.BootDirSpec{}
}

func (a *PlanScopedAdapter) ParseLineEvents(line []byte) ([]events.Event, error) {
	p, ok := a.Inner.(gop.EventParser)
	if !ok {
		return nil, nil
	}
	return p.ParseLineEvents(line)
}

func cloneCLIAdapter(adapter gop.CLIAdapter) gop.CLIAdapter {
	switch a := adapter.(type) {
	case *gop.ClaudeAdapter:
		clone := *a
		return &clone
	case interface{ Clone() gop.CLIAdapter }:
		return a.Clone()
	default:
		return adapter
	}
}
