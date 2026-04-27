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
// plan.Command and plan.Args are honored via the planScopedAdapter
// wrapper — the catalog declares the binary path + any prefix args
// (e.g. wrapper scripts), and the wrapper makes them stick across
// Detect()/BuildArgs without modifying go-providers' adapter.
//
// Stage 2a: this constructor is unused by app.Service — it stands
// alongside the existing internal/provider/cli/claudestream/adapter.go
// implementation. Stage 2b switches consumers over and deletes the
// custom adapter.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	inner := gop.NewClaudeAdapter()
	wrapped := &planScopedAdapter{
		inner:    inner,
		binary:   plan.Command,
		baseArgs: append([]string(nil), plan.Args...),
	}
	return agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{
		ID:      "claude-stream",
		Kind:    "cli",
		Adapter: wrapped,
		Caps: agentsessions.Capabilities{
			PTY:               false,
			Resize:            false,
			ProviderSessionID: true,
			CheckpointResume:  false,
			BinaryRequired:    true,
		},
	})
}

// planScopedAdapter wraps a go-providers CLIAdapter so the catalog-
// resolved binary path + prefix args stick. Detect returns the catalog's
// binary unconditionally (catalog is the source of truth — env-var
// fallbacks like CLAUDE_CLI_PATH from go-providers' default would
// confuse multi-launch tenants). BuildArgs prepends plan.Args before the
// adapter's per-turn argv so wrapper scripts and env-injecting prefixes
// work transparently.
type planScopedAdapter struct {
	inner    gop.CLIAdapter
	binary   string
	baseArgs []string
}

func (a *planScopedAdapter) Name() string { return a.inner.Name() }

func (a *planScopedAdapter) BuildArgs(prompt, systemPrompt, cliSessionID string) []string {
	out := append([]string(nil), a.baseArgs...)
	return append(out, a.inner.BuildArgs(prompt, systemPrompt, cliSessionID)...)
}

func (a *planScopedAdapter) ParseLine(line []byte) ([]gop.StreamEvent, error) {
	return a.inner.ParseLine(line)
}

func (a *planScopedAdapter) Detect() (string, bool) {
	if a.binary == "" {
		return a.inner.Detect()
	}
	return a.binary, true
}
