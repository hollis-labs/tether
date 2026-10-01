package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
)

// sandboxProtectHealth is the daemon's /health view of protection.
func sandboxProtectHealth(h app.ProtectionHealth) *daemon.SandboxProtectHealth {
	return &daemon.SandboxProtectHealth{
		Enabled:            h.Enabled,
		DisabledByOperator: h.DisabledByOperator,
		Reason:             h.Reason,
		BwrapChecked:       h.BwrapChecked,
		BwrapUsable:        h.BwrapUsable,
		BwrapError:         h.BwrapError,
	}
}

// logControlPlaneProtection records at daemon startup whether the agents it
// launches get Tether's catalog and run directories as read-only protected
// paths (CW-20261001-0142). Protection that is off, whether by the
// operator's TETHER_SANDBOX_PROTECT=0 or because the platform is not yet
// covered, is logged as a warning so it is never silent. So is protection
// that is on but cannot work, because bubblewrap is missing or cannot build a
// namespace: every launch Tether must sandbox is then refused.
func logControlPlaneProtection(logf func(string, ...any), h app.ProtectionHealth) {
	switch {
	case !h.Enabled:
		logf("WARN: control-plane protection %s", h.Reason)
	case h.BwrapChecked && !h.BwrapUsable:
		logf("WARN: control-plane protection is on but unusable: %s. Launches of every agent except Codex, which has its own sandbox, will be refused until bubblewrap works, or %s=0 is set in this daemon's environment to run agents unprotected", h.BwrapError, app.ProtectEnv)
	default:
		logf("control-plane protection %s", h.Reason)
	}
}

// checkSandboxProtect reports control-plane protection. It asks the running
// daemon, whose environment (a systemd unit's, say) is what decides
// protection, and not `mux doctor`'s own shell. Without a daemon to ask it
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
			fmt.Sprintf("unset %s in muxd's environment and restart the daemon to protect them", app.ProtectEnv))
	case !h.Enabled:
		return warn(name, fmt.Sprintf("%s (%s)", h.Reason, source), "")
	case h.BwrapChecked && !h.BwrapUsable:
		return fail(name, fmt.Sprintf("on, but bubblewrap cannot build the sandbox, so every launch except Codex's is refused (%s): %s", source, h.BwrapError),
			fmt.Sprintf("install bubblewrap and allow unprivileged user namespaces, or set %s=0 in muxd's environment to run agents unprotected", app.ProtectEnv))
	}
	return ok(name, fmt.Sprintf("%s (%s)", h.Reason, source))
}

// doctorSandboxProtect gathers the protection check's input: the daemon's
// own report when it is reachable, and otherwise this process's decision and
// a probe of this host.
func doctorSandboxProtect(cat *config.Catalog, catalogRoot string) checkResult {
	if cat != nil {
		if cfg, err := daemonConfigFromCatalog(cat); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if health, err := client.New(cfg.ListenAddr).Health(ctx); err == nil && health.SandboxProtect != nil {
				return checkSandboxProtect(health.SandboxProtect, true)
			}
		}
	}
	local := app.ControlPlaneProtection(runtime.GOOS, os.Getenv)
	return checkSandboxProtect(sandboxProtectHealth(
		app.ComputeProtectionHealth(local, runtime.GOOS, config.Expand(catalogRoot), app.ProbeBwrap)), false)
}
