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
// the typed event mapping, the auth-failure classifier and recognizing a
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
// the planter. HOME is never relocated: agy's live credential is an OAuth
// token in the macOS Keychain (its logs report "authenticated via keyring"),
// and its global config under ~/.gemini is shared with the desktop app.
//
// The adapter's Preflight is not forwarded: it stats ~/.gemini/oauth_creds.json,
// which is Gemini CLI's file, not agy's, so it both passes without an agy
// login and refuses a working one. The launch path instead plants a browser
// shim (see PlantBrowserShim) and relies on the forwarded IsNotAuthenticated
// classifier.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	adapter := gop.NewAntigravityAdapter()
	adapter.Permission = permission(plan.PermissionMode)
	return claudestream.NewWithAdapter(plan, adapter, "antigravity", agentsessions.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	},
		// until go-providers drops the oauth_creds.json stat (CW-20260930-0221 R1)
		claudestream.WithoutPreflight(),
	)
}

func permission(mode string) string {
	if mode == config.PermissionModeBypass {
		return "bypass"
	}
	return "accept-edits"
}
