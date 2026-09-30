package claudestream

import (
	"github.com/hollis-labs/agentkit/agentsessions"
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

// Preflight forwards to the inner adapter's Preflighter (agy refuses to
// start without its credentials rather than open a browser login).
func (a *PlanScopedAdapter) Preflight() error {
	if p, ok := a.Inner.(gop.Preflighter); ok {
		return p.Preflight()
	}
	return nil
}

// IsNotAuthenticated forwards to the inner adapter's AuthFailureClassifier.
func (a *PlanScopedAdapter) IsNotAuthenticated(stderrTail []byte) bool {
	c, ok := a.Inner.(gop.AuthFailureClassifier)
	return ok && c.IsNotAuthenticated(stderrTail)
}

// ResumeKeepsSessionID forwards to the inner adapter's
// SessionResumeVerifier; adapters without one report false, so a changed
// id is never read as a lost session for them.
func (a *PlanScopedAdapter) ResumeKeepsSessionID() bool {
	v, ok := a.Inner.(gop.SessionResumeVerifier)
	return ok && v.ResumeKeepsSessionID()
}

// IsSessionLost forwards to the inner adapter's SessionLostClassifier.
// agentsessions asks the adapter it was given, which is this wrapper, so
// without the forward a dead resume id would never be recognized. An inner
// adapter with no classifier never reports a lost session.
func (a *PlanScopedAdapter) IsSessionLost(stderrTail []byte) bool {
	c, ok := a.Inner.(gop.SessionLostClassifier)
	return ok && c.IsSessionLost(stderrTail)
}
