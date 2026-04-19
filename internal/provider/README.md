# internal/provider

Provider runtime contract — the abstraction that lets one daemon launch
sessions against many kinds of backends (PTY-based CLIs, API-streaming
agents, …).

**Purpose:** defines the `Runtime` and `Session` interfaces plus the
shared `BuildEnv` helper for composing child-process environments.
Keeps the runtime manager decoupled from any specific backend.

**Entry points:**

- `Runtime` interface (`provider.go`) — `ID() / Kind() / Prepare(ctx, plan)
  / Start(ctx, plan, opts)`. Implementations live in subpackages.
- `Session` interface — `Wait() / Stop(ctx) / SendInput / Health /
  CheckpointHints`. Returned by `Runtime.Start`.
- `StartOptions` — Workdir, LogPath, BootPrompt, BootMode, Fanout writer.
- `BuildEnv` (`env.go`) — merge-by-default env composition with
  whitelist + redact opt-ins. See [ADR TBD on env-merge default].
- Subpackages:
  - [`cli/claudecode`](cli/claudecode) — PTY-backed runtime for the
    Claude CLI.
  - [`api/stub`](api/stub) — no-op API-kind runtime (echoes input).

**Neighbors:**

- [`internal/runtime`](../runtime) composes `StartOptions.Fanout` from
  its attach broker; Manager owns the Session's lifecycle.
- [`internal/launch`](../launch) resolves the `launch.Plan` that is
  passed to `Runtime.Prepare` + `Runtime.Start`.
- [`internal/app`](../app) registers concrete Runtime implementations
  at startup.

**Gotchas:**

- **Fan-out lives in the Manager, not the Session.** If you feel the
  urge to add `Attach(ctx, w)` to `Session`, stop — it duplicates the
  attach broker and breaks CLI/API symmetry.
- `CheckpointHints()` is deliberately opaque until v0.0.3 Sprint
  v003-04 pins the shape. Return an empty slice if you have nothing.
- Env handling is **merge-by-default**. Only `passthrough` / `redact` /
  `overrides` alter the parent env. Do not reintroduce the v0.0.1
  env-replace behavior.
- `Kind()` distinguishes CLI vs API runtimes. The stub runtime returns
  `api`; new vendor runtimes pick the appropriate kind.
