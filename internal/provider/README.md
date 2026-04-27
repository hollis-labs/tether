# internal/provider

Thin helper package — env-policy plumbing only.

The runtime contract that used to live here (`Runtime`, `Session`,
`Capabilities`, `HealthStatus`, `LiveState`, `CheckpointHint`,
`StartOptions`, `Registry`) moved to
[`github.com/hollis-labs/go-agent-sessions/agentsessions`](https://github.com/hollis-labs/go-agent-sessions)
in `v005-03`. Mux composes adapters via `agentsessions.NewFromAdapter`
(turn-based subprocess) or by implementing `agentsessions.Runtime`
directly (PTY).

## What's left here

- `BuildEnv` (`env.go`) — merge-by-default env composition with
  whitelist + redact opt-ins. `app.Service` calls this once per
  `LaunchSession` to build `agentsessions.StartOptions.Env`.
- Adapter packages under `cli/` (`claudestream`, `claudecode`,
  `opencode`) and `api/stub` — each ships a `New(plan)` factory that
  returns an `agentsessions.Runtime` for app composition.

## Where to look for the contract

- Runtime / Session interfaces, capability flags, sentinel errors:
  `agentsessions.{Runtime, Session, Capabilities, ErrNoInputChannel,
  ErrSessionNotRunning, ErrTurnInFlight}`
- Compliance harness: `github.com/hollis-labs/go-agent-sessions/compliance`
- ADR addendums: `docs/adr/0004` (broker), `0006` (runtime contract),
  `0025` (compliance suite).

## Gotchas

- Env handling is **merge-by-default**. Only `passthrough` / `redact` /
  `overrides` alter the parent env.
- The catalog-driven `cli-goprovider` provider type is registered as a
  factory in `app.New` via `claudestream.NewWithAdapter` — there is no
  longer a `goprovider/` subpackage.
