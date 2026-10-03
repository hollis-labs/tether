//go:build !windows

package shimhost

import (
	"fmt"
	"os/exec"

	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
)

// PrepareProvider applies resolved sandbox and limit wrappers to the REAL
// provider, before it is placed. The policy MUST deny the per-session state
// directory to the provider: same-uid filesystem ownership alone cannot hide
// the descriptor or controller capability. Keep cleanup until the host exits.
// A caller requiring protection must pass that policy and treat any error as
// refusal of the shim path; the bridge is a separate privileged control client.
func PrepareProvider(spec shim.Launch, policy *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
	noop := func() {}
	if len(spec.Argv) == 0 {
		return spec, noop, fmt.Errorf("missing provider argv")
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // Authorized, tokenized provider argv.
	cmd.Dir = spec.Cwd
	cmd.Env = append([]string{}, spec.Env...)
	sandboxCleanup := noop
	if policy != nil {
		_, cleanup, err := sandbox.ApplyResolved(cmd, *policy)
		if err != nil {
			return spec, noop, fmt.Errorf("provider sandbox: %w", err)
		}
		if cleanup != nil {
			sandboxCleanup = cleanup
		}
	}
	limitCleanup, err := runner.ApplyResourceLimits(cmd, limits)
	if err != nil {
		sandboxCleanup()
		return spec, noop, fmt.Errorf("provider limits: %w", err)
	}
	spec.Argv = append([]string{cmd.Path}, cmd.Args[1:]...)
	spec.Env = append([]string{}, cmd.Env...)
	return spec, func() { limitCleanup(); sandboxCleanup() }, nil
}
