//go:build !darwin && !linux

package sandbox

import (
	"fmt"
	"os/exec"
)

// Apply returns an error on unsupported platforms. Per ADR 0013, a non-empty
// profile on an unsupported platform is a hard launch failure.
func Apply(cmd *exec.Cmd, p Profile, workspace string) error {
	return fmt.Errorf("sandboxing not supported on this platform (profile %q requested)", p.ID)
}
