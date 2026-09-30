// Package antigravity runs the Antigravity CLI (agy) as a Tether runtime.
package antigravity

import (
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
)

// New constructs an agentsessions.Runtime that drives agy one subprocess per
// turn through go-providers' AntigravityAdapter: `agy --output-format
// stream-json [--conversation <id>] -p=<prompt>`. The adapter owns the argv,
// the typed event mapping, the credentials preflight and recognizing a
// replaced conversation; see its doc for agy's headless behavior.
//
// The launch's permission posture picks the approval flags: "bypass" passes
// --dangerously-skip-permissions (as claude launches do), anything else runs
// in accept-edits, where file edits proceed and commands are denied. Denied
// actions surface as provider.permission_denied events rather than silent
// no-ops. agy's --sandbox is advisory and is not used.
//
// cwd is the planted boot dir, whose .agents/ workspace root carries the
// launch's skills and MCP plugin; the project is attached with --add-dir by
// the planter. HOME is never relocated: agy's OAuth credentials live under
// ~/.gemini.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	adapter := gop.NewAntigravityAdapter()
	adapter.Permission = permission(plan.PermissionMode)
	return claudestream.NewWithAdapter(plan, adapter, "antigravity", agentsessions.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	})
}

func permission(mode string) string {
	if mode == config.PermissionModeBypass {
		return "bypass"
	}
	return "accept-edits"
}
