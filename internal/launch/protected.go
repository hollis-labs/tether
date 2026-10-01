package launch

import "errors"

// ErrLaunchInsideProtectedPath refuses a launch whose agent would work
// inside one of Tether's write-protected directories (CW-20261001-0142).
// Every agent Tether wraps gets the catalog root, the daemon's run directory
// and the state database's directory as read-only ProtectedPaths, so an agent
// whose work directory or workspace lies inside one could not do its job; the
// sandbox would refuse it anyway, and this says why before anything starts.
var ErrLaunchInsideProtectedPath = errors.New("launch refused: the agent would write inside a protected directory")

// ErrACPLaunchUnprotected refuses an ACP launch (Copilot, Pi). Tether
// write-protects its catalog, run and state directories for every agent it
// wraps, and go-agent-wrapper's ACP launcher cannot enforce
// ProtectedPaths without a resolved sandbox policy until CW-20261001-0162
// adds a protect-only sandbox. Running the agent unprotected would be
// fail-open, so the launch is refused instead.
var ErrACPLaunchUnprotected = errors.New("ACP launches are refused: Tether write-protects its catalog, run and state directories for every agent, and go-agent-wrapper's ACP launcher cannot enforce that until CW-20261001-0162 adds a protect-only sandbox")

// ErrProtectionUnavailable refuses a launch while control-plane protection is
// on but this host cannot provide it: on Linux, bubblewrap is missing. The
// operator either installs it or turns protection off explicitly
// (TETHER_SANDBOX_PROTECT=0).
var ErrProtectionUnavailable = errors.New("launch refused: Tether write-protects its catalog, run and state directories for every agent, and this host cannot")

// ErrCodexSandboxWidened refuses a turn on a codex session that Tether left to
// codex's own sandbox when it launched, because something that shapes that
// sandbox has since changed: a project .codex/config.toml appeared in a work
// directory, or the config.toml in CODEX_HOME no longer has the shape Tether
// planted. The session cannot be re-wrapped, so the turn is refused
// (CW-20261001-0142).
var ErrCodexSandboxWidened = errors.New("turn refused: the codex session's own sandbox may have been widened since it launched")
