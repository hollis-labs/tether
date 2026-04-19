# ADR 0005: Environment Composition — Merge by Default

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-04 (Provider Contract), task T-v002-s04-01
**Deciders:** agent-mux v0.0.2 execution session

## Context

v0.0.1's `claudecode.Build` set `cmd.Env = <plan.Env>` — replacing the
parent process environment rather than merging with it. This silently
dropped `$PATH`, `$HOME`, `$SHELL`, and anything else the spawned agent
needed to function. Smoke tests that worked from a developer shell
broke in surprising ways under the daemon because the daemon has a
narrower base environment than an interactive login.

The fix shape had to answer three questions:

1. Is the parent env inherited by default, or opted into?
2. How does the operator express "only these variables pass through"?
3. How does the operator express "redact these (e.g., secrets) even if
   the passthrough is wide"?

## Decision

**Merge by default**, with opt-ins for narrower passthrough and for
redaction.

API (`provider.BuildEnv`):

```go
BuildEnv(mode, passthrough, redact, overrides, parent) []string
```

- `mode` — `merge` (default, also the fallback for empty / unknown
  values), `whitelist` (only keys in `passthrough`), `none` (empty
  parent — overrides only).
- `passthrough` — key allow-list in `whitelist` mode.
- `redact` — key deny-list applied *after* mode selection, even in
  `merge` mode. Explicit secret-stripping.
- `overrides` — key/value pairs always applied last; win against parent
  + passthrough.

Storage (`launch.Plan`):

- `plan.EnvMode`, `plan.EnvPassthrough`, `plan.EnvRedact` carry the
  operator's intent.
- `plan.Env` carries **only** explicit overrides — parent values no
  longer materialize into `launch_plans`. This keeps the persisted plan
  small and makes env composition a pure function of (plan, current
  parent env) at Start time, not a snapshot from resolve time.

Adapter contract: runtimes call `provider.BuildEnv(...)` fresh against
`os.Environ()` inside `Runtime.Start`, not `Runtime.Prepare`.

## Alternatives Considered

- **Keep env-replace + document it.** Rejected: surprise > convenience.
  The trap already burned one session.
- **Merge-always, no opt-outs.** Rejected: operators writing launch
  profiles for secrets-sensitive agents need redaction and narrow
  passthrough as first-class concepts.
- **Resolve-time snapshot.** Rejected: the resolve happens seconds to
  minutes before Start; env can drift (user exports, CI variable
  changes). Compose fresh at Start.

## Consequences

- Existing launch profiles keep working — empty / absent `EnvMode`
  collapses to `merge`, which is what implicit behavior was supposed
  to be all along.
- Secrets hygiene is explicit. A launch profile that redacts `API_KEY`
  surfaces the decision in the YAML, not in ambient shell config.
- Tests cover the five modes + overrides-win + edge cases (malformed
  parent entries, values with `=` in them). Flipping the default back
  to replace would require reversing this ADR with a new one.
- The plan's `Env` field is small (operator overrides only). DB rows
  stay compact; readers rebuild the full env fresh at Start.
