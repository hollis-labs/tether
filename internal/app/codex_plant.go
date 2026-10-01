package app

import (
	"strconv"
	"strings"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentlaunch/providerplant"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/registry"
)

// Planted codex policy (CW-20261001-0216).
//
// A codex launch carries its posture twice. The launch template's argv has the
// registry's overrides, `-c sandbox_mode="workspace-write" -c
// approval_policy="on-request"`, and codex lets those win. The config.toml
// planted into CODEX_HOME comes from the go-providers adapter's own fields, and
// providerplant.DefaultResolver does not set them from the posture: the adapter
// keeps its headless default, approval_policy "never". So the file said "never"
// while the argv said "on-request".
//
// The argv is what runs, so nothing was wrong at runtime, but the file misled
// anyone reading it, and a codex started from this boot dir WITHOUT the template
// argv (a manual resume, a tool) would run under "never". Under "never" codex
// refuses every MCP tool call outright ("MCP tool call requires approval, but
// approval policy is never", see codex_approval.go), so the agent would lose its
// tether tools. plantResolver makes the file say what the argv says, from the same
// registry mapping, so the two cannot drift apart.

// plantResolver is providerplant.DefaultResolver, with a codex adapter's
// approval_policy and sandbox_mode set from the launch's permission posture.
func plantResolver(compiled *agentlaunch.CompiledLaunch) (provider.BootDirProvider, error) {
	adapter, err := providerplant.DefaultResolver(compiled)
	if err != nil {
		return nil, err
	}
	codex, ok := adapter.(*provider.CodexAdapter)
	if !ok {
		return adapter, nil
	}
	sandbox, approval := codexPostureSettings(compiled.Plan)
	if sandbox != "" {
		codex.SandboxMode = sandbox
	}
	if approval != "" {
		codex.ApprovalPolicy = approval
	}
	return codex, nil
}

// codexPostureSettings returns the sandbox_mode and approval_policy the registry
// maps the launch's posture to, read from the `-c key="value"` overrides it puts
// in the argv. Either is "" when the launch carries no posture or the registry
// has no mapping for it, which leaves the adapter's own default.
func codexPostureSettings(plan *agentlaunch.LaunchPlan) (sandbox, approval string) {
	if plan == nil || plan.Provider.Permission == "" {
		return "", ""
	}
	desc, ok := registry.Lookup(plan.Provider.ID)
	if !ok {
		return "", ""
	}
	launch, err := desc.PostureFor(plan.Provider.Permission, plan.Runtime)
	if err != nil {
		return "", ""
	}
	for i := 0; i+1 < len(launch.Args); i++ {
		if launch.Args[i] != "-c" {
			continue
		}
		key, quoted, found := strings.Cut(launch.Args[i+1], "=")
		if !found {
			continue
		}
		value, err := strconv.Unquote(quoted)
		if err != nil {
			continue
		}
		switch key {
		case "sandbox_mode":
			sandbox = value
		case "approval_policy":
			approval = value
		}
	}
	return sandbox, approval
}
