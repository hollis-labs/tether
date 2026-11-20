package gatewaylaunch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hollis-labs/libs/harness/core/sandbox"
)

// CheckHidden rejects constraints that would conceal either credential. It never
// removes a deny or hide rule to make the gateway reachable.
func CheckHidden(paths []string, tokenPath, workspace string) error {
	for _, raw := range paths {
		if raw == "workspace" {
			raw = workspace
		}
		home, _ := os.UserHomeDir()
		raw = strings.ReplaceAll(raw, "${HOME}", home)
		if raw == "~" {
			raw = home
		} else if strings.HasPrefix(raw, "~/") {
			raw = filepath.Join(home, raw[2:])
		}
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(workspace, raw)
		}
		raw = filepath.Clean(raw)
		raw = resolveAncestors(raw)
		for _, target := range []string{tokenPath, RunTokenPath(tokenPath)} {
			target = resolveAncestors(target)
			rel, err := filepath.Rel(raw, target)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return errors.New("gateway credentials conflict with sandbox hide or deny rules")
			}
		}
	}
	return nil
}

// ReadableProfile mounts this session's directory after the legacy private /dev
// mount, preserving atomic refresh without exposing other sessions' credentials.
func ReadableProfile(p sandbox.Profile, tokenPath, workspace string) (sandbox.Profile, error) {
	if err := CheckHidden(append(slices.Clone(p.FS.Hide), p.FS.Deny...), tokenPath, workspace); err != nil {
		return p, err
	}
	if p.ID != "" && !p.HostFilesystem {
		p.FS.Read = append(slices.Clone(p.FS.Read), filepath.Dir(tokenPath))
	}
	return p, nil
}

// CheckResolvedPolicy rejects confinement whose pinned builder mounts private
// /dev after access grants, hiding tmpfs. The library must preserve the grant
// before these policies can support gateway launches.
func CheckResolvedPolicy(p *sandbox.ResolvedAccessPolicy) error {
	if p != nil && p.Mode != sandbox.ConfinementDisabled {
		return errors.New("gateway credentials do not support resolved sandbox confinement")
	}
	return nil
}

// Resolve existing ancestors even if the final credential was atomically
// removed after expiry or a hide names a not-yet-created run.token.
func resolveAncestors(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolveAncestors(parent), filepath.Base(path))
}
