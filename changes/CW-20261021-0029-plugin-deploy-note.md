- Document that hosted Claude sessions load the tether-mail plugin from the copy on their own
  host (CW-20261021-0029): `docs/messaging.md` now says a plugin release reaches hosted sessions
  only when that host's copy (on the worker host, `~/.claude/skills/tether-mail`) is updated, and
  that a running session needs a relaunch to get the `TETHER_HOSTED_SESSION` marker and drop its
  stale watcher. Found live: the worker host's copy predated the stand-down, so the plugin kept
  duplicating the hub wake.
