package app

import (
	"strconv"
	"strings"

	"github.com/hollis-labs/substrate/harness/adapters/provider"
	"github.com/hollis-labs/substrate/harness/adapters/registry"
	"github.com/hollis-labs/substrate/harness/agentlaunch"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"
)

// A codex launch carries its posture in both the template argv and its
// planted CODEX_HOME/config.toml. Resolve both from the registry so a manual
// resume from the boot dir uses the same policy. Default sessions use
// workspace-write/on-request; explicit bypass uses danger-full-access/never
// (CW-20261001-0251). Never refuses approval-gated MCP calls in a sandboxed
// session, but full-access bypass permits the planted MCP calls on 0.159.3.

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
