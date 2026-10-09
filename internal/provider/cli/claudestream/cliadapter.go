package claudestream

import (
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	events "github.com/hollis-labs/substrate/harness/adapters/provider/events"
	llmtypes "github.com/hollis-labs/substrate/llm-core/llmtypes"

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
func NewWithAdapter(plan *launch.Plan, adapter gop.CLIAdapter, providerID string, caps agentsessions.Capabilities, opts ...Option) (agentsessions.Runtime, error) {
	return agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{
		ID:      providerID,
		Kind:    "cli",
		Adapter: NewPlanScopedAdapter(plan, adapter, opts...),
		Caps:    caps,
	})
}

// NewPlanScopedAdapter constructs the same adapter used by NewWithAdapter,
// preserving only the interrupt interfaces the inner adapter implements.
// Capability queries can pass a nil plan to inspect this shape without
// resolving a launch or running preflight.
func NewPlanScopedAdapter(plan *launch.Plan, adapter gop.CLIAdapter, opts ...Option) gop.CLIAdapter {
	scoped := &PlanScopedAdapter{Inner: adapter}
	if plan != nil {
		scoped.Binary = plan.Command
		scoped.BaseArgs = append([]string(nil), plan.Args...)
	}
	for _, opt := range opts {
		opt(scoped)
	}
	return scoped.withInterrupts()
}

// Option adjusts the PlanScopedAdapter NewWithAdapter builds.
type Option func(*PlanScopedAdapter)

// WithoutPreflight stops the wrapper forwarding the inner adapter's
// Preflighter, so Prepare no longer refuses on the adapter's own check.
func WithoutPreflight() Option {
	return func(a *PlanScopedAdapter) { a.SkipPreflight = true }
}

// PlanScopedAdapter wraps a go-providers CLIAdapter so the catalog-
// resolved binary path + prefix args stick. Detect returns the catalog's
// binary unconditionally (catalog is the source of truth — env-var
// fallbacks like CLAUDE_CLI_PATH from go-providers' default would
// confuse multi-launch tenants). BuildArgs prepends Plan.Args before
// the adapter's per-turn argv, but only a runtime started without a launch
// template calls it: a launched session's every turn comes from the shared
// launch's template (StartOptions.Launch), which puts the catalog's flags
// at the convention's extra-argument slot. A catalog arg meant as a prefix
// ahead of the provider's own argv (a wrapper script's arguments) is
// therefore not supported on the launch path. BootDirSpec is forwarded so lib v0.9.x's
// AutoPlantBootDir path can discover the inner adapter's planting spec
// through the wrapper.
type PlanScopedAdapter struct {
	Inner    gop.CLIAdapter
	Binary   string
	BaseArgs []string

	// SkipPreflight suppresses the Preflight forward. Classifiers such as
	// IsNotAuthenticated are still forwarded.
	SkipPreflight bool
}

// withInterrupts preserves optional interrupt interfaces without claiming
// them on adapters that do not implement them. Their methods have no neutral
// unsupported response, so putting them on every PlanScopedAdapter would
// advertise a capability the inner adapter does not have.
func (a *PlanScopedAdapter) withInterrupts() gop.CLIAdapter {
	stream, hasStream := a.Inner.(gop.TurnInterrupter)
	rpc, hasRPC := a.Inner.(gop.RPCTurnInterrupter)
	switch {
	case hasStream && hasRPC:
		return &bothInterruptAdapter{PlanScopedAdapter: a, TurnInterrupter: stream, RPCTurnInterrupter: rpc}
	case hasStream:
		return &streamInterruptAdapter{PlanScopedAdapter: a, TurnInterrupter: stream}
	case hasRPC:
		return &rpcInterruptAdapter{PlanScopedAdapter: a, RPCTurnInterrupter: rpc}
	default:
		return a
	}
}

type streamInterruptAdapter struct {
	*PlanScopedAdapter
	gop.TurnInterrupter
}

type rpcInterruptAdapter struct {
	*PlanScopedAdapter
	gop.RPCTurnInterrupter
}

type bothInterruptAdapter struct {
	*PlanScopedAdapter
	gop.TurnInterrupter
	gop.RPCTurnInterrupter
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

// Preflight forwards to the inner adapter's Preflighter unless
// SkipPreflight is set.
func (a *PlanScopedAdapter) Preflight() error {
	if a.SkipPreflight {
		return nil
	}
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
