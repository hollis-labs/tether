# Sprint v002-04 — Provider Contract

Epic: [v0.0.2](../epics/v0.0.2-runtime-foundation.md)

**Epic:** [v0.0.2](../epics/v0.0.2-runtime-foundation.md)
**Goal:** Evolve the provider interface so both CLI (PTY-backed) and API (streaming-SDK) runtimes satisfy the same high-level session contract. Fix the aggressive env replacement. Introduce `internal/provider/api/` as scaffold (no concrete SDK implementation — that's v0.0.3). Keep the existing `claudecode` adapter green throughout.
**Exit criteria:**
- [x] `provider.Adapter` expresses `prepare / start / stop / send-input / attach-stream / health` semantics per context-pack §08. → Replaced by `provider.Runtime` + `provider.Session` (Wait/Stop/SendInput/Health/CheckpointHints). Attach fan-out is owned by the runtime manager via `StartOptions.Fanout`; Sessions don't need an Attach method.
- [x] A concrete CLI-runtime implementation (`internal/provider/cli/claudecode`, renamed) conforms.
- [x] `internal/provider/api/` exists with interface scaffolding and a typed placeholder (no vendor SDK wired yet). → `internal/provider/api/stub` implements echo-style Runtime/Session; deletable once v0.0.3 Anthropic/OpenAI adapter lands.
- [x] Env handling merges passthrough + overrides onto the inherited base, not blind replacement.
- [x] Existing demo-launch smoke test still passes.

## Context

Context-pack §08 "Provider contract" calls for a common runtime contract supporting both CLI and API execution modes with behaviors: prepare, start, stop, send input/message, attach/read stream, checkpoint hints, health. Context-pack §06 §4 calls out the env handling bug: "The current Claude Code adapter replaces the environment instead of merging controlled overrides with inherited base environment." Context-pack §07 targets `internal/provider/cli/claudecode`, `internal/provider/api/openai`, `internal/provider/api/anthropic` as target paths.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/provider.go` — minimal `Adapter` interface: `ID()`, `Build(plan, workdir) (*exec.Cmd, error)`. Assumes exec.Cmd; tied to PTY.
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/claudecode/adapter.go` — `flattenEnv` produces `cmd.Env = []string{...}` from `plan.Env` alone. No inherited base.
- `/Users/chrispian/Projects-apps/agent-mux/internal/launch/resolver.go:727-732` — builds `plan.Env` from passthrough + overrides; passes to adapter which then replaces the full env.

When done, adding a second runtime mode (API-streaming) doesn't require breaking the Adapter interface again.

## Tasks

### T-v002-s04-01: Fix env handling (passthrough + overrides merged onto base)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [bug, provider, env, foundation]

#### Problem

The current `claudecode.Adapter.Build` sets `cmd.Env = flattenEnv(plan.Env)`. This overwrites the entire child environment, so the child loses anything not listed under `passthrough`. Git, npm, SSH, homebrew paths, everything — all gone unless explicitly listed.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/internal/provider/claudecode/adapter.go`:
```go
func (Adapter) Build(plan *launch.Plan, workdir string) (*exec.Cmd, error) {
    ...
    cmd.Env = flattenEnv(plan.Env)
    return cmd, nil
}
```
And in `internal/launch/resolver.go`:
```go
env := map[string]string{}
for _, k := range prov.Env.Passthrough {
    if v, ok := os.LookupEnv(k); ok { env[k] = v }
}
for k, v := range l.Overrides.Env { env[k] = v }
```
Passthrough gate is applied at resolve time; adapter then treats the map as the *whole* env.

#### Fix direction

Two changes:
1. **Semantics shift:** `plan.Env` becomes "additions and overrides on top of the child's inherited env." Passthrough semantics stop being a whitelist and become a *redaction* list (start from `os.Environ()`, drop anything the user wants scrubbed, then overlay plan.Env).
2. **Adapter change:** `cmd.Env = mergedEnv(os.Environ(), plan.Env, plan.EnvRedact)`.

If context-pack authors intended passthrough-as-whitelist for isolation, we keep that mode opt-in via a new `env.mode: whitelist|merge` field (default: merge).

Default behavior: merge with whole parent env. Whitelist remains available.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/claudecode/adapter.go` — fix the env composition
- `/Users/chrispian/Projects-apps/agent-mux/internal/launch/resolver.go` — stop pre-flattening; pass both passthrough and overrides through plan
- `/Users/chrispian/Projects-apps/agent-mux/internal/launch/plan.go` — add `EnvMode`, `EnvRedact`
- `/Users/chrispian/Projects-apps/agent-mux/internal/config/model.go` — `ProviderEnv.Mode` field (default "merge")
- `/Users/chrispian/Projects-apps/agent-mux/examples/catalog/providers/claude-code.yaml` — adjust if needed to document the default

#### Acceptance criteria

- [x] Default-mode launched children see `$PATH`, `$HOME`, `$SHELL`, etc. inherited from the daemon process.
- [x] Opt-in `env.mode: whitelist` preserves the old behavior (only listed keys forwarded).
- [x] Unit tests cover both modes and redaction.
- [x] `mux launch --launch demo-launch` still succeeds against the existing demo setup.

#### Test plan

- Unit: `merge` mode produces a superset of parent env with overrides applied; `whitelist` mode produces only listed keys.
- Unit: redaction drops listed keys from the merged result.
- Integration: launch a session that runs `/usr/bin/env` (via a test provider) and asserts $PATH is present.

#### Scope fences

- Do not change the YAML config *schema* beyond adding `env.mode` — keep back-compat.
- Do not audit every provider YAML in the catalog for redactions — that's a configuration concern.
- Do not leak secrets through logs: merged env should not be written to `events` table.

#### Relationship

Siblings: T-v002-s04-02 (interface evolution also touches adapter).

#### Origin

Context-pack [06-review-of-current-v0.md](../agent-mux-vfuture-context-pack/06-review-of-current-v0.md) §4, [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md) ("env handling that merges passthrough plus overrides, not blind replacement").

---

### T-v002-s04-02: Evolve `Adapter` interface to support CLI and API modes

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, provider, contract]

#### Problem

The current interface assumes a PTY child process. It returns `*exec.Cmd`. An API-backed runtime (streaming Anthropic/OpenAI SDK) doesn't have an `exec.Cmd` to return. The contract has to become about session lifecycle, not process construction.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/internal/provider/provider.go`:
```go
type Adapter interface {
    ID() string
    Build(plan *launch.Plan, workdir string) (*exec.Cmd, error)
}
```

#### Fix direction

Evolve to a richer contract (concrete Go signatures to be refined during implementation):

```go
type Runtime interface {
    ID() string
    Kind() RuntimeKind  // "cli" | "api"
    Prepare(ctx context.Context, plan *launch.Plan) error
    Start(ctx context.Context, plan *launch.Plan) (Session, error)
}

type Session interface {
    Stop(ctx context.Context) error
    SendInput(ctx context.Context, data []byte) error
    Attach(ctx context.Context, w io.Writer) error  // live stream
    Health() HealthStatus
    CheckpointHints() (CheckpointHint, bool)  // optional
}
```

CLI implementations wrap PTY; API implementations wrap a streaming SDK call. Both satisfy `Session`.

Keep the old `Adapter` interface as a deprecated thin shim for one release — the refactor shouldn't be a big-bang rewrite. `runtime.Manager` (from v002-s01-01) should talk to the new interface.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/provider.go` — introduce `Runtime`, `Session`, `RuntimeKind`, `HealthStatus`
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/cli/` (new subpackage path; move `claudecode` here per context-pack §07)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/cli/claudecode/adapter.go` — refactor to satisfy `Runtime` and `Session`
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` — consume new interface
- `/Users/chrispian/Projects-apps/agent-mux/internal/app/service.go` — update provider registration

#### Acceptance criteria

- [x] `Runtime` and `Session` interfaces defined with the listed methods.
- [x] `claudecode` satisfies both.
- [x] `runtime.Manager` uses `Runtime.Start → Session` instead of building `*exec.Cmd` directly.
- [x] Demo launch end-to-end still works.
- [x] The old `Adapter` interface is either removed or clearly deprecated with a doc comment. → removed; replaced by `Runtime` + `Session`.

#### Test plan

- Unit: interface conformance check via blank assignment `var _ provider.Runtime = (*claudecode.Adapter)(nil)`.
- Integration: demo launch smoke path.

#### Scope fences

- Do not implement an API-mode runtime here — that's T-v002-s04-03 (scaffold) + T-v003-03-01 (first real impl).
- Do not change the YAML provider config schema beyond what's needed for `kind: cli|api`.
- Do not generalize `Session` to support arbitrary streams (audio, etc.). Text only.

#### Relationship

Depends on: T-v002-s01-01 (runtime manager needs to consume the new interface).
Blocks: T-v002-s04-03, T-v003-03-01.

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) Provider contract, [07-proposed-package-boundaries.md](../agent-mux-vfuture-context-pack/07-proposed-package-boundaries.md).

---

### T-v002-s04-03: Scaffold `internal/provider/api/` with a stub implementation

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, provider, api]

#### Problem

An API-backed runtime is a new shape. Even without wiring a real SDK in v0.0.2, having a concrete (stub) implementation proves the `Runtime` / `Session` interface works for the non-PTY case and gives v0.0.3 a clear starting point.

#### Fix direction

- Create `internal/provider/api/` with a `stub` subpackage.
- `stub.Runtime` implements `Runtime` and returns a `stub.Session` on Start.
- `stub.Session` maintains an in-memory channel; SendInput appends to the channel; Attach streams back out (echo-style).
- Register the stub in provider registry under id `api-stub`; add a YAML catalog entry so `mux resolve --launch api-stub-launch` works.

This is intentionally throwaway — it proves the interface works for a non-process runtime and becomes an integration test harness.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/api/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/api/stub/stub.go`
- `/Users/chrispian/Projects-apps/agent-mux/examples/catalog/providers/api-stub.yaml` (new)
- `/Users/chrispian/Projects-apps/agent-mux/examples/catalog/launches/api-stub-launch.yaml` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/app/service.go` — register stub

#### Acceptance criteria

- [x] `mux resolve --launch api-stub-launch` returns a plan with `provider_id: api-stub`.
- [x] `mux launch --launch api-stub-launch` creates a session that immediately reaches `running` state without spawning a process.
- [x] `mux sessions input <id> "hello"` → attach stream returns `echo: hello`.
- [x] `mux sessions stop <id>` transitions to terminal state.

#### Test plan

- Unit: end-to-end against the stub — start, send, attach, verify, stop.
- Integration: same flow via daemon + CLI.

#### Scope fences

- Do not wire Anthropic or OpenAI SDK — that's v0.0.3.
- Do not invent a generic "API provider config schema" beyond what the stub needs. Broad schema work is premature until the first real provider exists.
- Do not make the stub a long-term fixture — it can be deleted when the first real API provider exists. Document this.

#### Relationship

Depends on: T-v002-s04-02.
Blocks: T-v003-03-01 (first real API provider).

#### Origin

Context-pack [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md) (v0.0.2: "API runtime adapter interface is introduced"), [07-proposed-package-boundaries.md](../agent-mux-vfuture-context-pack/07-proposed-package-boundaries.md).

---

### T-v002-s04-04: Migrate `claudecode` to `internal/provider/cli/claudecode`

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [refactor, provider, paths]

#### Problem

Current path `internal/provider/claudecode/` doesn't match the context-pack §07 target `internal/provider/cli/claudecode`. After T-v002-s04-02 the interface changes anyway, so co-locating the move makes sense.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/internal/provider/claudecode/adapter.go` — current location. Context-pack §07 proposes `internal/provider/cli/claudecode`.

#### Fix direction

- `git mv internal/provider/claudecode/ internal/provider/cli/claudecode/`
- Update import paths in `internal/app/service.go`.
- Update the `ID()` string if needed (probably leave as `claude-code`).
- Keep existing tests passing.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/cli/claudecode/adapter.go` (post-move)
- `/Users/chrispian/Projects-apps/agent-mux/internal/app/service.go`

#### Acceptance criteria

- [x] Directory moved; imports updated.
- [x] `go build ./...` and `go test ./...` pass.
- [x] Smoke test still works.

#### Test plan

- Build + test.

#### Scope fences

- Pure path refactor. Do not change semantics in this task.

#### Relationship

Depends on: T-v002-s04-02.
Pairs with: T-v002-s04-03.

#### Origin

Context-pack [07-proposed-package-boundaries.md](../agent-mux-vfuture-context-pack/07-proposed-package-boundaries.md).

## Review / readiness notes

- **Env mode default:** the choice to default to `merge` is a usability call, but it's a security surface. If the pack readers had a strict-whitelist design in mind, flip the default. Readiness review should confirm.
- **SendInput on API sessions** — what does "input" mean for a streaming SDK session? Probably "append a user message" (Anthropic/OpenAI conversation model). The interface can't perfectly abstract this; the `api-stub` concretizes "bytes in, bytes out" for v0.0.2, and the first real API provider (v0.0.3) refines the semantics.
- **CheckpointHints** is listed in context-pack §08 but has no defined schema yet. Stub as `interface{}` or a typed opaque blob in v0.0.2; pin the shape in v0.0.3 alongside Sprint v003-04.
