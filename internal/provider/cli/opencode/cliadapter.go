package opencode

import (
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
)

// New constructs an agentsessions.Runtime that drives `opencode run
// --format json` through go-providers' OpencodeAdapter, which owns the
// argv (`run --format json --agent ... [--session <id>] <prompt>`), the
// typed event mapping (text, tool use, per-step usage, done at the end of
// the turn), and recognizing a lost resume id on stderr.
//
// Agent is the planted agent (launch.OpencodeAgentName, namespaced so it
// cannot merge into one of opencode's built-in agents), the --agent the
// shared launch's projection names; its agents/<name>.md carries the boot
// prompt. A launched session's argv comes from agentkit's launch template
// (CW-20260930-0135), which already names it; the adapter's own BuildArgs
// does the same when it runs without one.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	adapter := gop.NewOpencodeAdapter()
	adapter.Agent = launch.OpencodeAgentName(plan)
	return claudestream.NewWithAdapter(planWithoutRunSubcommand(plan), adapter, "opencode", agentsessions.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	})
}

// planWithoutRunSubcommand drops a leading "run" from plan.Args. Catalogs
// seeded before the adapter owned the subcommand declare `args: [run]`,
// and PlanScopedAdapter prepends plan.Args to the adapter's argv, which
// would otherwise start `run run ...`. launch.CatalogFlags is the one
// definition; the shared launch uses it too.
func planWithoutRunSubcommand(plan *launch.Plan) *launch.Plan {
	p := *plan
	p.ProviderBrand = "opencode"
	p.Args = launch.CatalogFlags(&p)
	return &p
}
