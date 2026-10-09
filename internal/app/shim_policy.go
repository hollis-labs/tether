//go:build !windows

package app

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/tether/internal/shimhost"
)

// Preserve the legacy implicit workspace bind when translating to resolved
// confinement. A missing requested path is a distinct pre-placement refusal.
func shimSandboxPolicy(opts agentsessions.StartOptions, privateRoot string) (sandbox.ResolvedAccessPolicy, error) {
	privateRoot = realPathOrClean(privateRoot)
	protected := append(append([]string{}, opts.ProtectedPaths...), filepath.Dir(privateRoot))
	var policy sandbox.ResolvedAccessPolicy
	var err error
	if opts.SandboxPolicy != nil {
		policy = *opts.SandboxPolicy
		policy.FS.Deny = append(append([]sandbox.ResolvedPath{}, policy.FS.Deny...), sandbox.ResolvedPath{Kind: sandbox.AccessDeny, Path: realPathOrClean(privateRoot)})
		policy, err = policy.WithProtected(protected...)
	} else {
		profile := opts.Profile
		if profile.ID == "" {
			profile = sandbox.Profile{ID: "shim-provider", HostFilesystem: true, Net: true, Subprocess: true}
		}
		profile.FS.Protect = append(append([]string{}, profile.FS.Protect...), protected...)
		profile.FS.Deny = append(append([]string{}, profile.FS.Deny...), privateRoot)
		profile.DenyGUILaunch = profile.DenyGUILaunch || opts.DenyGUILaunch
		request := sandbox.PolicyFromProfile(profile, opts.Workdir)
		grant := opts.Workdir
		if profile.HostFilesystem {
			grant = "/"
		}
		request.FS.Write = append(request.FS.Write, sandbox.PathRef{Path: grant})
		policy, err = sandbox.ResolveAccessPolicy(request)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return policy, &shimhost.Failure{Code: "policy_path_missing", Message: "requested policy path unavailable before placement"}
		}
		return policy, &shimhost.Failure{Code: "sandbox_unavailable", Message: "policy cannot protect private shim state"}
	}
	if policy.Mode == sandbox.ConfinementDisabled || len(policy.Network.LoopbackPorts) > 0 {
		return policy, &shimhost.Failure{Code: "sandbox_unavailable", Message: "policy requires daemon-owned resources or cannot protect private shim state"}
	}
	policy.DenyGUILaunch = policy.DenyGUILaunch || opts.DenyGUILaunch
	policy.DenyUserServiceManager = true
	for _, denied := range policy.FS.Deny {
		if _, err := os.Lstat(denied.Path); err != nil {
			return policy, &shimhost.Failure{Code: "policy_path_missing", Message: "requested deny path unavailable before placement"}
		}
	}
	return policy, nil
}

// Private shim state must be hidden from other Tether-sandboxed launches too.
// Unsandboxed launches remain unsandboxed; the flag-off path is untouched.
func (s *Service) applyShimSandboxProtection(kind string, opts *agentsessions.StartOptions) error {
	if s.LaunchHost() != HostShim || runtime.GOOS != "linux" || kind == "api" {
		return nil
	}
	sandboxed := opts.Profile.ID != "" || len(opts.ProtectedPaths) > 0 || opts.SandboxPolicy != nil && opts.SandboxPolicy.Mode != sandbox.ConfinementDisabled
	if !sandboxed {
		return nil
	}
	host, err := s.shimHost()
	if err != nil {
		return err
	}
	root := filepath.Dir(host.provider.SessionDir("private-root"))
	policy, err := shimSandboxPolicy(*opts, root)
	if err != nil {
		return err
	}
	opts.SandboxPolicy = &policy
	opts.Profile = sandbox.Profile{}
	return nil
}
