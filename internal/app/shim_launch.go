//go:build !windows

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/shimhost"
)

type shimHosting struct {
	place      func(context.Context, string, shim.Launch) (shimhost.Receipt, error)
	stop       func(context.Context, shimhost.Receipt) error
	provider   *shimhost.Provider
	bridge     []string
	instance   string
	prepare    func(shim.Launch, *sandbox.ResolvedAccessPolicy, runner.ResourceLimits) (shim.Launch, func(), error)
	capability func() error
	inspect    func(context.Context, shimhost.Receipt) (shimhost.Inspection, error)
	cleanup    sync.Map
}

func (h *shimHosting) placeProvider(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
	if h.place != nil {
		return h.place(ctx, key, spec)
	}
	return h.provider.Place(ctx, key, spec)
}
func (h *shimHosting) stopProvider(ctx context.Context, receipt shimhost.Receipt) error {
	if h.stop != nil {
		return h.stop(ctx, receipt)
	}
	return h.provider.Stop(ctx, receipt)
}

func trustedShimEnv() []string {
	var env []string
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (s *Service) shimHost() (*shimHosting, error) {
	s.shimMu.Lock()
	defer s.shimMu.Unlock()
	if s.shimHosting != nil {
		return s.shimHosting, nil
	}
	if s.Catalog == nil {
		return nil, &shimhost.Failure{Code: "unsupported", Message: "catalog unavailable"}
	}
	root := filepath.Join(filepath.Dir(config.ResolveStateDB(s.Catalog.Global.Catalog.Defaults, s.Catalog.Paths)), "shims")
	if !filepath.IsAbs(root) {
		return nil, &shimhost.Failure{Code: "unsupported", Message: "private shim root unavailable"}
	}
	command, err := os.Executable()
	if err != nil {
		return nil, &shimhost.Failure{Code: "unsupported", Message: "host executable unavailable"}
	}
	cfg := s.Catalog.Global.Catalog.Defaults.ShimHost
	backend := shimhost.Detached
	if cfg.SystemdUser {
		backend = shimhost.SystemdUser
	}
	p, err := shimhost.New(shimhost.Config{StateDir: root, ShimCommand: []string{command, "shim-host"}, HostEnv: trustedShimEnv(), Backend: backend, UnitPrefix: cfg.UnitPrefix, AllowSystemd: cfg.SystemdUser, JournalBytes: cfg.EffectiveJournalBytes()})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(root))
	s.shimHosting = &shimHosting{provider: p, bridge: []string{command, "shim-bridge"}, instance: "urn:instance:" + hex.EncodeToString(digest[:]), prepare: shimhost.PrepareProvider, capability: shimhost.Supported, inspect: p.Inspect}
	return s.shimHosting, nil
}

// prepareShimStart leaves the request untouched on every pre-placement fallback.
// Once a placement may exist, errors retain its identity and forbid another child.
func (s *Service) prepareShimStart(ctx context.Context, plan *launch.Plan, req agentsessions.StartRequest) (agentsessions.StartRequest, error) {
	if s.LaunchHost() != HostShim {
		return req, nil
	}
	fallback := func(code string) (agentsessions.StartRequest, error) {
		s.shimDiagnostic(req.ID, nil, "direct_fallback", code)
		return req, nil
	}
	if plan.ProviderBrand != "claude" || !req.Runtime.Caps().StreamingStdio || req.Options.PreparedExecution != nil || req.Options.Launch == nil || req.Options.Launch.Convention.Mode != runtimes.ModeStreamingStdio || len(req.Options.ExtraFiles) != 0 || req.Options.Supervisor != nil {
		return fallback("unsupported_runtime")
	}
	host, err := s.shimHost()
	if err != nil {
		return fallback(shimFailureCode(err))
	}
	if err = host.capability(); err != nil {
		return fallback(shimFailureCode(err))
	}
	spec, policy, limits, err := shimProviderSpec(host, plan, req)
	if err != nil {
		return fallback(shimFailureCode(err))
	}
	spec, cleanup, err := host.prepare(spec, policy, limits)
	if err != nil {
		return fallback("sandbox_unavailable")
	}
	// A scope wrapper owns a separate unit and does not provide the shim's
	// process identity. Refuse it before placement rather than weaken teardown.
	if strings.HasSuffix(spec.Argv[0], "systemd-run") {
		cleanup()
		return fallback("unsupported_limits")
	}
	intent := host.provider.PlacementIdentity(req.ID+":1", spec)
	if err = s.persistShim(ctx, req.ID, plan.ProviderID, spec.BootGeneration, intent); err != nil {
		cleanup()
		return fallback("placement_bookkeeping_unavailable")
	}
	// Make the durable intent eligible for startup recovery before submission.
	if err = s.Store.UpdateSessionState(req.ID, string(session.StateLaunching), 0, nil); err != nil {
		cleanup()
		return req, &shimhost.Failure{Code: "outcome_unknown", Message: "placement intent state could not be recorded"}
	}
	if err = initializeShimCheckpoint(intent); err != nil {
		cleanup()
		_ = s.MarkSessionDetached(req.ID, shimFailureCode(err))
		return req, err
	}
	receipt, err := host.placeProvider(ctx, intent.OperationKey, spec)
	if err != nil && !receipt.Attempted {
		var saved shimhost.Receipt
		readErr := shimhost.ReadPrivateJSON(filepath.Join(filepath.Dir(intent.DescriptorPath), "placement.json"), shim.MaxFrame, &saved)
		if readErr == nil && saved.Attempted {
			receipt = saved
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			receipt = intent
			receipt.Attempted = true
			err = &shimhost.Failure{Code: "outcome_unknown", Message: "existing placement receipt unreadable"}
		}
	}
	if err != nil && (!receipt.Attempted || receipt.PlacementFailure == "placement_failed" || receipt.Retired) {
		cleanup()
		if removeErr := s.Store.RemoveUnstartedSessionShim(ctx, req.ID, intent.OperationKey); removeErr != nil {
			return req, &shimhost.Failure{Code: "outcome_unknown", Message: "unstarted placement bookkeeping could not be cleared"}
		}
		_ = s.Store.UpdateSessionState(req.ID, string(session.StateCreated), 0, nil)
		return fallback(shimFailureCode(err))
	}
	if receipt.Attempted {
		host.cleanup.Store(req.ID, cleanup)
		if saveErr := s.persistShim(ctx, req.ID, plan.ProviderID, spec.BootGeneration, receipt); saveErr != nil {
			err = &shimhost.Failure{Code: "outcome_unknown", Message: "placement exists but its database identity could not be saved"}
		}
	}
	if err != nil {
		_ = s.Store.UpdateSessionState(req.ID, string(session.StateLaunching), 0, nil)
		_ = s.MarkSessionDetached(req.ID, shimFailureCode(err))
		s.shimDiagnostic(req.ID, &receipt, "detached", shimFailureCode(err))
		return req, err
	}
	if e := initializeShimCheckpoint(receipt); e != nil {
		_ = s.MarkSessionDetached(req.ID, "outcome_unknown")
		return req, &shimhost.Failure{Code: "outcome_unknown", Message: "existing bridge checkpoint unavailable"}
	}
	bridge, err := s.shimBridgeRuntime(req.ID, plan.ProviderID, host.bridge, req.Runtime.Caps())
	if err != nil {
		return req, &shimhost.Failure{Code: "outcome_unknown", Message: "placed host has no bridge runtime"}
	}
	req.Runtime = bridge
	req.Options = shimBridgeOptions(req.Options, host.bridge, receipt, false)
	return req, nil
}

func shimProviderSpec(host *shimHosting, plan *launch.Plan, req agentsessions.StartRequest) (shim.Launch, *sandbox.ResolvedAccessPolicy, runner.ResourceLimits, error) {
	opts := req.Options
	dir := host.provider.SessionDir(req.ID)
	if err := shimhost.PrivateDir(dir); err != nil {
		return shim.Launch{}, nil, runner.ResourceLimits{}, err
	}
	binary, err := exec.LookPath(plan.Command)
	if err != nil {
		return shim.Launch{}, nil, runner.ResourceLimits{}, err
	}
	systemPrompt := opts.BootPrompt
	if opts.BootMode == "stdin" {
		systemPrompt = ""
	}
	args, err := opts.Launch.TurnArgv(gop.TurnInput{SystemPrompt: systemPrompt, ResumeID: opts.SessionIDPreset}, opts.ExtraArgs...)
	if err != nil {
		return shim.Launch{}, nil, runner.ResourceLimits{}, err
	}
	profile := opts.Profile
	if profile.ID == "" {
		profile = sandbox.Profile{ID: "shim-provider", HostFilesystem: true, Net: true, Subprocess: true}
	}
	privateRoot := filepath.Dir(dir)
	protected := append(append([]string{}, opts.ProtectedPaths...), filepath.Dir(privateRoot))
	profile.FS.Protect = append(append([]string{}, profile.FS.Protect...), protected...)
	profile.FS.Deny = append(append([]string{}, profile.FS.Deny...), privateRoot)
	profile.DenyGUILaunch = profile.DenyGUILaunch || opts.DenyGUILaunch
	profile.DenyUserServiceManager = true
	request := sandbox.PolicyFromProfile(profile, opts.Workdir)
	if profile.HostFilesystem {
		request.FS.Write = append(request.FS.Write, sandbox.PathRef{Path: "/"})
	}
	policy, err := sandbox.ResolveAccessPolicy(request)
	if opts.SandboxPolicy != nil {
		policy = *opts.SandboxPolicy
		policy.FS.Deny = append(append([]sandbox.ResolvedPath{}, policy.FS.Deny...), sandbox.ResolvedPath{Kind: sandbox.AccessDeny, Path: privateRoot})
		policy, err = policy.WithProtected(protected...)
		policy.DenyGUILaunch = policy.DenyGUILaunch || opts.DenyGUILaunch
		policy.DenyUserServiceManager = true
	}
	if err != nil || policy.Mode == sandbox.ConfinementDisabled || len(policy.Network.LoopbackPorts) > 0 {
		return shim.Launch{}, nil, runner.ResourceLimits{}, &shimhost.Failure{Code: "sandbox_unavailable", Message: "provider policy requires daemon-owned resources or cannot deny the shim state"}
	}
	pin := filepath.Join(dir, "boot.pin")
	f, err := os.OpenFile(pin, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // Private pin path is derived from the provider session directory.
	if err == nil {
		err = f.Close()
	}
	if err != nil && !errors.Is(err, os.ErrExist) {
		return shim.Launch{}, nil, runner.ResourceLimits{}, err
	}
	boot := sha256.Sum256([]byte(req.ID + "\x00" + opts.BootContent + "\x00" + opts.BootPrompt))
	spec := shim.Launch{Session: req.ID, Instance: host.instance, Generation: 1, Actor: mesh.Actor{URN: mesh.URN(registry.LogicalAgentBindingTarget(plan.LogicalAgentID)), Kind: mesh.ActorAgent}, Subject: mesh.URN("urn:session:" + req.ID), Argv: append([]string{binary}, args...), Env: append([]string{}, opts.Env...), Cwd: opts.Workdir, PinPath: pin, PinKey: req.ID, BootGeneration: hex.EncodeToString(boot[:]), Reservation: req.ID + ":1", Heartbeat: time.Second}
	var limits runner.ResourceLimits
	if opts.ResourceLimits != nil {
		limits = runner.ResourceLimits(*opts.ResourceLimits)
	}
	return spec, &policy, limits, nil
}

func (s *Service) shimBridgeRuntime(sessionID, id string, command []string, caps agentsessions.Capabilities) (agentsessions.Runtime, error) {
	adapter := &shimClaudeAdapter{ClaudeAdapter: gop.NewClaudeAdapterStreamingStdio(), service: s, sessionID: sessionID}
	adapter.Binary = command[0]
	return agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{ID: id, Kind: "cli", Adapter: adapter, Caps: caps})
}

func shimBridgeOptions(opts agentsessions.StartOptions, command []string, receipt shimhost.Receipt, attach bool) agentsessions.StartOptions {
	args := append(append([]string{}, command[1:]...), "--descriptor", receipt.DescriptorPath)
	if attach {
		args = append(args, "--attach", "--takeover", "--journal", receipt.Journal)
		opts.AutoFireFirstTurn = false
		opts.FirstTurnPayload = nil
		opts.BootContent = ""
		opts.BootPrompt = ""
		opts.BootMode = "none"
	}
	var template []gop.ArgTemplate
	for _, arg := range args {
		template = append(template, gop.ArgTemplate{Kind: gop.ArgLiteral, Value: arg})
	}
	opts.Launch = &agentlaunch.TurnTemplate{Convention: gop.LaunchConvention{Executable: command[0], Mode: runtimes.ModeStreamingStdio, Argv: template}}
	opts.ExtraArgs = nil
	opts.Env = trustedShimEnv()
	opts.Profile = sandbox.Profile{}
	opts.SandboxPolicy = nil
	opts.PreparedExecution = nil
	opts.ProtectedPaths = nil
	opts.DenyGUILaunch = false
	opts.ResourceLimits = nil
	opts.Supervisor = nil
	return opts
}
