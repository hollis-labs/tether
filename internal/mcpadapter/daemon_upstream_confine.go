package mcpadapter

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/tether/internal/config"
)

// daemonUpstreamEnvironment inherits only portable process basics. Catalog
// entries explicitly supply application settings and resolved credentials;
// unrelated daemon credentials, service-manager and loader knobs stay private.
func daemonUpstreamEnvironment(inherited []string, configured map[string]string) []string {
	env := []string{}
	for _, value := range inherited {
		name, _, _ := strings.Cut(value, "=")
		switch name {
		case "PATH", "HOME", "LANG", "TMPDIR", "LC_ALL", "LC_CTYPE", "LC_COLLATE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME", "LC_PAPER", "LC_NAME", "LC_ADDRESS", "LC_TELEPHONE", "LC_MEASUREMENT", "LC_IDENTIFICATION":
			env = append(env, value)
		}
	}
	for name, value := range configured {
		env = append(env, name+"="+value)
	}
	return env
}

// DaemonProtectedRoots names all three control-plane trees. Unlike a planted
// proxy, a daemon pool must protect them regardless of the caller's sandbox or
// TETHER_SANDBOX_PROTECT. Missing roots fail closed before any upstream starts; a
// project's broken catalog entry does not (see paths).
type DaemonProtectedRoots struct {
	Catalog       string
	Run           string
	State         string
	CatalogConfig *config.Catalog // loaded catalog determines all authority-bearing layers
}

func (r DaemonProtectedRoots) paths() ([]string, error) {
	if r.CatalogConfig == nil {
		return nil, fmt.Errorf("confine daemon MCP upstream: loaded catalog is required for layer protection")
	}
	var out []string
	for _, root := range []struct{ name, path string }{{"catalog", r.Catalog}, {"run", r.Run}, {"state", r.State}} {
		if !filepath.IsAbs(root.path) {
			return nil, fmt.Errorf("confine daemon MCP upstream: %s root must be absolute and present", root.name)
		}
		resolved, err := filepath.EvalSymlinks(root.path)
		if err != nil {
			return nil, fmt.Errorf("confine daemon MCP upstream: %s root: %w", root.name, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("confine daemon MCP upstream: %s root must be a directory", root.name)
		}
		if resolved == string(filepath.Separator) {
			return nil, fmt.Errorf("confine daemon MCP upstream: %s root cannot be the filesystem root", root.name)
		}
		out = append(out, resolved)
	}
	// Tolerant: a project whose catalog entry is broken (its repo_root runs through a
	// file or a symlink an agent could replace, cannot be examined) must not stop
	// the gateway from building. That layer is left out of the confinement of the
	// upstreams' children, loudly; everything else is protected.
	prepared, err := config.PrepareCatalogProtectionWith(r.Catalog, r.CatalogConfig, config.ProtectionOptions{Tolerate: true})
	if err != nil {
		return nil, err
	}
	for _, open := range prepared.Unprotected {
		log.Printf("WARN: protect: project %q: its layer cannot be protected, and is LEFT OPEN to the daemon's MCP upstream children (the gateway keeps running; launches of agents Tether protects are refused until it is fixed): its repo_root %s %s. Fix or remove the project", open.Project, open.RepoRoot, open.Why)
	}
	out = append(out, prepared.Dirs...)
	return out, nil
}

// confineDaemonUpstream preserves host reads/network/workspace writes but makes
// the control plane read-only. Every daemon stdio launch takes this path; no
// sandbox error is permitted to fall back to the original command.
func confineDaemonUpstream(cmd *exec.Cmd, protected []string) error {
	profile := sandbox.Profile{ID: "tether-daemon-mcp-control-plane", HostFilesystem: true,
		Net: true, Subprocess: true, DenyUserServiceManager: true,
		FS: sandbox.FSSpec{Protect: protected}}
	cleanup, err := sandbox.Apply(cmd, profile, string(filepath.Separator))
	if err != nil {
		return fmt.Errorf("confine daemon MCP upstream: %w", err)
	}
	cleanup() // protect-only does not allocate runtime resources
	return nil
}
