# ADR 0006: Provider Contract — Runtime + Session Split, Opaque CheckpointHints

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-04 (Provider Contract), task T-v002-s04-02
**Deciders:** agent-mux v0.0.2 execution session

## Context

v0.0.1 had a single `provider.Adapter` interface that conflated two
distinct lifecycles: the *preparation* of a launch (validate the
command, resolve paths, ensure the binary exists) and the *runtime* of
a live session (wait, stop, send input, health, checkpoint hints).
Everything was tied to a PTY-backed CLI runtime.

v0.0.2 needed to:

1. Support API-streaming runtimes (e.g., future vendor SDKs) alongside
   the CLI path, from day one, without a branching `if kind == "cli"`.
2. Let the daemon `Prepare` a launch well before committing to `Start`
   (Sprint v002-05's create/launch split — see ADR 0009).
3. Reserve the eventual checkpoint-hint payload without locking a schema.

## Decision

Split the provider contract into two interfaces:

```go
type Runtime interface {
    ID() string
    Kind() string                  // "cli" | "api" (extensible)
    Prepare(ctx, plan) error       // idempotent, side-effect-light
    Start(ctx, plan, opts) (Session, error)
}

type Session interface {
    Wait() (exitCode int, err error)
    Stop(ctx) error
    SendInput(ctx, data []byte) error
    Health() SessionHealth
    CheckpointHints() []CheckpointHint
}
```

- `StartOptions` carries Workdir / LogPath / BootPrompt / BootMode /
  Fanout (an `io.Writer` wired by the runtime manager).
- `CheckpointHint` is a deliberately opaque struct — its shape is not
  pinned until v0.0.3 Sprint v003-04. Today, runtimes may return
  empty slices.
- Subpackages: `internal/provider/cli/claudecode/` (PTY-backed),
  `internal/provider/api/stub/` (no-op echo, establishes the API path).

## Alternatives Considered

- **Keep one `Adapter` interface, dispatch by `Kind()`.** Rejected:
  forces every call site to branch, and the Prepare/Start split has
  no clean expression.
- **Three interfaces (Prepare, Runtime, Session).** Rejected: the
  Prepare step is small and naturally belongs with the Runtime it
  produces.
- **Pin `CheckpointHint` shape now.** Rejected: v0.0.2 does no
  checkpoint resume. Pinning the shape before a consumer exists is
  speculative; the spec pack flags v003-04 as the ADR-authoring window.

## Consequences

- The CLI and API paths are symmetric. A new vendor runtime implements
  `Runtime` + `Session` and gets attach, input, events, and lifecycle
  semantics for free from `runtime.Manager`.
- v0.0.1's `provider.Adapter` / `runtime.Starter` / `runtime.Handle` /
  `runtime.ErrNoPTYWriter` were deleted outright — pre-launch, no
  compat shim. See feedback memory on no-compat-shims.
- `CheckpointHints()` is an interface method returning a slice of
  opaque structs. Callers that need the payload today (v0.0.2
  checkpoints are storage-only) just persist the opaque bytes.
- Interface stability is a promise: breaking changes to `Runtime` or
  `Session` require a new ADR that supersedes this one. Additive
  changes (new methods via interface embedding + adapter) are fine.
- The `Kind()` string is the extension point. New values do not
  require an ADR; they require a concrete runtime, catalog wiring, and
  the manager treating them identically.
