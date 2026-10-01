package claudestream

import (
	"slices"

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
func NewWithAdapter(plan *launch.Plan, adapter gop.CLIAdapter, providerID string, caps agentsessions.Capabilities, opts ...Option) (agentsessions.Runtime, error) {
	scoped := &PlanScopedAdapter{
		Inner:    adapter,
		Binary:   plan.Command,
		BaseArgs: append([]string(nil), plan.Args...),
	}
	for _, opt := range opts {
		opt(scoped)
	}
	rt, err := agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{
		ID:      providerID,
		Kind:    "cli",
		Adapter: scoped,
		Caps:    caps,
	})
	if err != nil {
		return nil, err
	}
	return &Runtime{Runtime: rt, adapter: scoped}, nil
}

// Runtime is the agentsessions.Runtime NewWithAdapter builds, holding its
// per-session PlanScopedAdapter so the launch can hand it the arguments only
// the launch knows once the runtime exists (SetExtraArgs). agentsessions
// never type-asserts a Runtime, so the embedding hides nothing it reads.
type Runtime struct {
	agentsessions.Runtime
	adapter *PlanScopedAdapter
}

// SetExtraArgs sets the launch-only arguments for every turn of this
// session; see PlanScopedAdapter.SetExtraArgs. Call it before Start.
func (r *Runtime) SetExtraArgs(args []string) { r.adapter.SetExtraArgs(args) }

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
// the adapter's per-turn argv so wrapper scripts and env-injecting
// prefixes work transparently. BootDirSpec is forwarded so lib v0.9.x's
// AutoPlantBootDir path can discover the inner adapter's planting spec
// through the wrapper.
type PlanScopedAdapter struct {
	Inner    gop.CLIAdapter
	Binary   string
	BaseArgs []string

	// SkipPreflight suppresses the Preflight forward. Classifiers such as
	// IsNotAuthenticated are still forwarded.
	SkipPreflight bool

	// extraArgs are the launch-only arguments (the planted --mcp-config,
	// the project's --add-dir/--cd, injected args) that app.sharedExtraArgs
	// picks out of the shared launch. Set once, before Start.
	extraArgs []string
}

// SetExtraArgs sets the launch-only arguments BuildArgs adds to every turn.
//
// INTERIM (CW-20261001-0065): CW-20260930-0106 replaces it with one
// prepared execution. go-providers v0.34.1 ends a per-turn argv with an
// end-of-options "--" and the prompt (CW-20261001-0069), so that turn text
// can never be parsed as a flag. agentsessions still appends
// StartOptions.ExtraArgs after the whole argv, where they would land after
// "--" and become prompt text or stray positionals (codex exec exits 2 on
// one). BuildArgs splices them in before the "--" instead. The inner adapter
// is shared by every session of its provider, so its own ExtraArgs field
// cannot carry per-session values.
func (a *PlanScopedAdapter) SetExtraArgs(args []string) {
	a.extraArgs = append([]string(nil), args...)
}

func (a *PlanScopedAdapter) Name() string { return a.Inner.Name() }

func (a *PlanScopedAdapter) BuildArgs(prompt, systemPrompt, cliSessionID string) []string {
	out := append([]string(nil), a.BaseArgs...)
	return append(out, beforeEndOfOptions(a.Inner.BuildArgs(prompt, systemPrompt, cliSessionID), a.extraArgs)...)
}

// beforeEndOfOptions inserts extra ahead of the first "--" in argv, the
// end-of-options marker go-providers puts before a turn's prompt, or appends
// it when argv has none (streaming-stdio, PTY, app-server and agy's inline
// -p=<prompt> take no trailing prompt). The convention's own options never
// include a bare "--", so the first one is the marker even when the prompt
// is itself "--".
func beforeEndOfOptions(argv, extra []string) []string {
	if len(extra) == 0 {
		return argv
	}
	i := slices.Index(argv, "--")
	if i < 0 {
		return append(argv, extra...)
	}
	return slices.Concat(argv[:i], extra, argv[i:])
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
