package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
)

// sandboxProtectHealth is the daemon's /health view of protection.
func sandboxProtectHealth(h app.ProtectionHealth) *daemon.SandboxProtectHealth {
	return &daemon.SandboxProtectHealth{
		Enabled:            h.Enabled,
		DisabledByOperator: h.DisabledByOperator,
		Reason:             h.Reason,
		Codex:              h.Codex.State,
		CodexReason:        h.Codex.Reason,
		BwrapChecked:       h.BwrapChecked,
		BwrapUsable:        h.BwrapUsable,
		BwrapError:         h.BwrapError,
	}
}

// logControlPlaneProtection records at daemon startup whether the agents it
// launches get Tether's catalog, run and state directories as read-only protected
// paths (CW-20261001-0142). Protection that is off, whether by the
// operator's TETHER_SANDBOX_PROTECT=0 or because the platform is not yet
// covered, is logged as a warning so it is never silent. So is protection
// that is on but cannot work, because bubblewrap is missing or cannot build a
// namespace: every launch Tether must sandbox (not Codex's) is then refused.
func logControlPlaneProtection(logf func(string, ...any), h app.ProtectionHealth) {
	switch {
	case !h.Enabled:
		logf("WARN: control-plane protection %s", h.Reason)
	case h.BwrapChecked && !h.BwrapUsable:
		logf("WARN: control-plane protection is on but unusable: %s. Launches of Claude, OpenCode and every agent Tether wraps will be refused until bubblewrap works, or %s=0 is set in this daemon's environment to run agents unprotected; Codex launches are not affected", h.BwrapError, app.ProtectEnv)
	default:
		logf("control-plane protection %s", h.Reason)
	}
}

// logCodexProtection records at daemon startup how codex is protected
// (guarded, or the not-protected fallback), naming CW-20261001-0230 in either
// case: it is the structural reason (codex spawns MCP servers outside its
// sandbox). The fallback is a WARN, so it is never silent.
func logCodexProtection(logf func(string, ...any), h app.ProtectionHealth) {
	if h.Codex.State == "" || !h.Enabled {
		return
	}
	if h.Codex.State == "guarded" {
		logf("codex protection %s", h.Codex.Reason)
		return
	}
	logf("WARN: codex protection %s", h.Codex.Reason)
}

// checkCodexProtection reports how codex is protected, as the daemon says: ok
// when guarded, a warning when not protected (the fallback), and ok with the
// reason when protection is off, since sandbox-protect already warns then. The
// text is honest in both modes and names CW-20261001-0230.
func checkCodexProtection(h *daemon.SandboxProtectHealth, fromDaemon bool) checkResult {
	const name = "sandbox-protect-codex"
	source := "daemon"
	if !fromDaemon {
		source = "this shell's environment; the daemon is not running or does not report it"
	}
	switch h.Codex {
	case "":
		return ok(name, fmt.Sprintf("not reported by the daemon (%s): it predates the codex protection report", source))
	case "not protected":
		return warn(name, fmt.Sprintf("codex: %s (%s)", h.CodexReason, source), "legacy_proxy launches keep MCP children outside Codex sandboxing; daemon ownership confines upstreams under the daemon (CW-20261001-0230); verify the launch ownership and the remaining Codex protection limits")
	}
	return ok(name, fmt.Sprintf("codex: %s (%s)", h.CodexReason, source))
}

// checkSandboxProtect reports control-plane protection. It asks the running
// daemon, whose environment (a systemd unit's, say) is what decides
// protection, and not `tether doctor`'s own shell. Without a daemon to ask it
// falls back to this process's environment and says so.
//
// Protection that is off warns; protection that is on but cannot work fails,
// since the launches Tether must sandbox are then refused.
func checkSandboxProtect(h *daemon.SandboxProtectHealth, fromDaemon bool) checkResult {
	const name = "sandbox-protect"
	source := "daemon"
	if !fromDaemon {
		source = "this shell's environment; the daemon is not running or does not report it"
	}
	switch {
	case !h.Enabled && h.DisabledByOperator:
		return warn(name, fmt.Sprintf("%s (%s)", h.Reason, source),
			fmt.Sprintf("unset %s in tetherd's environment and restart the daemon to protect them", app.ProtectEnv))
	case !h.Enabled:
		return warn(name, fmt.Sprintf("%s (%s)", h.Reason, source), "")
	case h.BwrapChecked && !h.BwrapUsable:
		return fail(name, fmt.Sprintf("on, but bubblewrap cannot build the sandbox, so Claude, OpenCode and every agent Tether wraps will be refused; Codex launches are not affected (%s): %s", source, h.BwrapError),
			fmt.Sprintf("install bubblewrap and allow unprivileged user namespaces, or set %s=0 in tetherd's environment to run agents unprotected", app.ProtectEnv))
	}
	return ok(name, fmt.Sprintf("%s (%s)", h.Reason, source))
}

// doctorSandboxProtect gathers the protection check's input: the daemon's
// own report when it is reachable, and otherwise this process's decision and
// a probe of this host. It returns the protection check and the codex check.
func doctorSandboxProtect(cat *config.Catalog, catalogRoot string) []checkResult {
	if cat != nil {
		if cfg, err := daemonConfigFromCatalog(cat); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if health, err := daemonClient(cfg.ListenAddr).Health(ctx); err == nil && health.SandboxProtect != nil {
				return []checkResult{checkSandboxProtect(health.SandboxProtect, true), checkCodexProtection(health.SandboxProtect, true)}
			}
		}
	}
	local := app.ControlPlaneProtection(runtime.GOOS, os.Getenv)
	h := sandboxProtectHealth(app.ComputeProtectionHealth(local, runtime.GOOS, config.Expand(catalogRoot), app.ProbeBwrap))
	return []checkResult{checkSandboxProtect(h, false), checkCodexProtection(h, false)}
}
