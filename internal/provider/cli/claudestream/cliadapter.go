package claudestream

import (
	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	llmtypes "github.com/hollis-labs/go-llm-types"
	gop "github.com/hollis-labs/go-providers/provider"
	events "github.com/hollis-labs/go-providers/provider/events"

	"github.com/hollis-labs/tether/internal/launch"
)

// New constructs an agentsessions.Runtime that drives the claude CLI's
// `--print --output-format stream-json` mode through go-providers'
// ClaudeAdapter, wrapped in a PlanScopedAdapter so the catalog-resolved
// binary path + prefix args stick. Per-session boot-dir planting is
// handled by go-agent-sessions v0.9.x via StartOptions.AutoPlantBootDir
// at LaunchSession time — this constructor only builds the runtime.
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
// catalog-driven cli-goprovider entries (codex, claude) which follow the
// same per-turn subprocess + stream-JSON pattern as claude.
func NewWithAdapter(plan *launch.Plan, adapter gop.CLIAdapter, providerID string, caps agentsessions.Capabilities) (agentsessions.Runtime, error) {
	return agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{
		ID:   providerID,
		Kind: "cli",
		Adapter: &PlanScopedAdapter{
			Inner:    adapter,
			Binary:   plan.Command,
			BaseArgs: append([]string(nil), plan.Args...),
		},
		Caps: caps,
	})
}

// PlanScopedAdapter wraps a go-providers CLIAdapter so the catalog-
// resolved binary path + prefix args stick. Detect returns the catalog's
// binary unconditionally (catalog is the source of truth — env-var
// fallbacks like CLAUDE_CLI_PATH from go-providers' default would
// confuse multi-launch tenants). BuildArgs prepends Plan.Args before
// the adapter's per-turn argv so wrapper scripts and env-injecting
// prefixes work transparently. BootDirSpec is forwarded so lib v0.9.x's
// AutoPlantBootDir path can discover the inner adapter's planting spec
// through the wrapper.
type PlanScopedAdapter struct {
	Inner    gop.CLIAdapter
	Binary   string
	BaseArgs []string
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
