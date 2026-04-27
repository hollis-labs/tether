package claudestream

import (
	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"

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
// catalog-driven cli-goprovider entries (codex, aider, gemini, …) which
// follow the same per-turn subprocess + stream-JSON pattern as claude.
func NewWithAdapter(plan *launch.Plan, adapter gop.CLIAdapter, providerID string, caps agentsessions.Capabilities) (agentsessions.Runtime, error) {
	wrapped := &PlanScopedAdapter{
		Inner:    adapter,
		Binary:   plan.Command,
		BaseArgs: append([]string(nil), plan.Args...),
	}
	return agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{
		ID:      providerID,
		Kind:    "cli",
		Adapter: wrapped,
		Caps:    caps,
	})
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

func (a *PlanScopedAdapter) Name() string { return a.Inner.Name() }

func (a *PlanScopedAdapter) BuildArgs(prompt, systemPrompt, cliSessionID string) []string {
	out := append([]string(nil), a.BaseArgs...)
	return append(out, a.Inner.BuildArgs(prompt, systemPrompt, cliSessionID)...)
}

func (a *PlanScopedAdapter) ParseLine(line []byte) ([]gop.StreamEvent, error) {
	return a.Inner.ParseLine(line)
}

func (a *PlanScopedAdapter) Detect() (string, bool) {
	if a.Binary == "" {
		return a.Inner.Detect()
	}
	return a.Binary, true
}
