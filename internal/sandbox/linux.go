//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Apply wraps cmd to run under bwrap (bubblewrap) if available.
// Returns an error if bwrap is not found — per ADR 0013, no silent downgrade.
func Apply(cmd *exec.Cmd, p Profile, workspace string) error {
	bwrapBin, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf("bwrap not found: cannot enforce profile %q on this system (install bubblewrap)", p.ID)
	}

	args, err := BuildBwrapArgs(p, workspace)
	if err != nil {
		return err
	}

	origPath := cmd.Path
	origArgs := cmd.Args
	cmd.Path = bwrapBin
	cmd.Args = append([]string{"bwrap"}, append(args, append([]string{origPath}, origArgs[1:]...)...)...)
	return nil
}

// BuildBwrapArgs translates a Profile into bwrap CLI arguments.
func BuildBwrapArgs(p Profile, workspace string) ([]string, error) {
	home, _ := os.UserHomeDir()
	args := []string{
		"--die-with-parent",
		"--proc", "/proc",
		"--dev", "/dev",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/sbin", "/sbin",
		"--ro-bind", "/etc", "/etc",
		"--tmpfs", "/tmp",
	}

	// Write paths (read+write bind)
	seen := map[string]bool{}
	for _, raw := range p.FS.Write {
		path := expandPathLinux(raw, workspace, home)
		if seen[path] {
			continue
		}
		seen[path] = true
		args = append(args, "--bind", path, path)
	}
	// Workspace always writable
	if !seen[workspace] {
		args = append(args, "--bind", workspace, workspace)
	}

	// Read-only paths
	for _, raw := range p.FS.Read {
		path := expandPathLinux(raw, workspace, home)
		if seen[path] || path == workspace {
			continue
		}
		seen[path] = true
		args = append(args, "--ro-bind-try", path, path)
	}

	// Network: deny all outbound via unshare-net
	if !p.Net {
		args = append(args, "--unshare-net")
	}

	return args, nil
}

func expandPathLinux(path, workspace, home string) string {
	if path == "workspace" {
		return workspace
	}
	path = strings.ReplaceAll(path, "${HOME}", home)
	path = strings.ReplaceAll(path, "~", home)
	return path
}
