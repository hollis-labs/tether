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
// Agent stays empty, so opencode runs its default agent as it always has
// here: the boot prompt already reaches the model through the catalog's
// prepend bootstrap, and selecting the planted agents/<id>.md would put it
// in the system prompt a second time.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	return claudestream.NewWithAdapter(planWithoutRunSubcommand(plan), gop.NewOpencodeAdapter(), "opencode", agentsessions.Capabilities{
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
