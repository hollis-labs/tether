- Core for the per-agent activity adapters (CW-20261014-0143, slice 1): a new `internal/agentevents` package records
  `harness/core` runtimeevents v1.1 events (subject, source channel, `exact`/`derived`/`inferred` confidence tier, causation and
  actor blocks, validated against the envelope rules) into a local per-session JSONL spool under a private directory. The spool
  assigns the per-session sequence under a file lock so it stays monotonic across the separate processes hooks run in, skips torn
  lines on read, and bounds itself: a session file past its cap loses its oldest lines (counted in a dropped counter) and creating a session prunes idle or excess sessions. It reads no vendor, plants no hook and talks to no hub yet; the Claude, Codex and Agy
  adapters and the relay build on it. `harness/core` moves to v0.11.0 (and `mesh/core` to v0.5.0) for the v1.1 envelope.
