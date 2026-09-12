# ADR 0046: PTY Runtime Removal — Tether's Consumer Is An Agent, Not A Person

**Status:** Accepted — 2026-09-12
**Supersedes:** ADR-0031 (TUI Removal)
**Related:** ADR-0035 (MCPAdapter Daemon-Client Routing), ADR-0039 (boot-exec Claude-Only Scope), ADR-0037 (Provider Runtime Kind Matrix)

## Context

ADR 0031 removed Tether's own Bubble Tea TUI in 2026-05. It drew the line in
the right place for what it could see at the time — "Mux is a daemon-shaped
session control plane whose product surfaces are CLI / MCP / HTTP / ACP /
GUI" — but it removed only the interface Tether *rendered*. It left behind the
runtime kind that exists to serve an interactive human at a terminal: `pty`.

Eighteen months of portfolio work have since made the app seams far more
definite than they were when 0031 was written. The decision that follows from
those seams was made but never recorded, so the PTY runtime survived on
inertia rather than on a rationale anyone could point at.

**Tether is an agent-facing tool.** Its consumers are agents and the programs
that orchestrate them. Its GUI exists so a person can *manage a Tether
instance* — inspect sessions, read events, stop something that is misbehaving
— which is a different act from *being the party the service serves*. A human
who wants to drive a live interactive coding session uses **Tachyon**. That is
Tachyon's job, and it is not Tether's.

The PTY runtime is the last structural claim that the opposite is true. What
it costs today:

- **It contradicts the boundary in code.** `internal/config/provider_runtime.go`
  declares `RuntimeKindPTY`, `internal/app/runtime_resolver.go` resolves it
  while logging `WARN: resolving deprecated PTY runtime ... prefer
  streaming_stdio or subprocess`, and four catalog launch profiles still ride
  it. A deprecation warning that nothing retires is not a decision, it is a
  deferral.
- **It is the reason first-turn handling is special-cased.**
  `deferPTYStdinBootPrompt` (`internal/app/session_lifecycle.go`) is Tether's
  ONLY call into `sessionkit.ApplyFirstTurnPolicy`, and it exists to work
  around a PTY-specific input-buffer deadlock. Because that workaround is
  PTY-shaped, the general capability it wraps was never surfaced for the
  runtimes agents actually use — which is CW-20260911-0093, still open.
  Removing PTY removes the reason the special case exists.
- **It pays for a capability nothing agent-facing consumes.** PTY carries
  resize plumbing (ADR-0014), raw-input attach semantics, and a distinct
  session type in agentkit. Every one of those is surface an agent never
  touches.
- **It misleads by existing.** `boot-exec` is already Claude-TUI-only by
  ADR-0039, and `-tui` launch profiles read as though driving Tether
  interactively is a supported product path. It is not.

The framed-turn runtimes are strictly better for the consumer Tether actually
has. `streaming-stdio` and `jsonrpc-stdio` both accept a submitted turn, both
report capabilities honestly, and both are what `mux sessions turn`, the MCP
`mux_session_send_turn` tool, and mailbox wake injection already drive.

## Decision

**Remove the `pty` runtime kind from Tether entirely.**

1. Retire `RuntimeKindPTY` from the config vocabulary. A launch profile or
   provider naming it fails validation with a message pointing at
   `streaming-stdio` and Tachyon, rather than resolving with a warning.
2. Remove the PTY branch from runtime resolution and the `claude-pty`
   provider from the reference catalog.
3. Migrate the four `-tui` launch profiles, or delete them. They describe a
   use case that belongs to Tachyon.
4. Remove `deferPTYStdinBootPrompt` and the PTY-specific first-turn
   workaround. The general first-turn capability is CW-20260911-0093 and is
   designed against framed runtimes.
5. Reassess the resize endpoint (ADR-0014) once PTY is the only thing that
   needed it.

This supersedes ADR 0031. 0031's decision — remove the rendered TUI — stands
and is not reopened; this record extends its reasoning to the runtime that
served the same consumer, and states the boundary 0031 could only gesture at.

`boot-exec` is deliberately out of scope. It execs into the native Claude CLI
and hands the terminal to the user, which is an explicit, documented, opt-in
escape hatch (ADR-0039) rather than Tether hosting an interactive session. It
is not the PTY runtime and does not depend on it.

## Consequences

**Gained.** One consumer, stated once. An agent-facing control plane whose
runtime matrix contains only runtimes agents can be driven through. The
first-turn special case loses its reason to exist, unblocking a clean general
implementation. Less capability surface (resize, raw attach, a session type)
maintained for nobody.

**Lost.** Tether can no longer host an interactive human terminal session.
That is the intent: Tachyon owns that. Anyone relying on a `-tui` launch
profile must move to Tachyon or to `boot-exec`.

**Risk.** The four `-tui` profiles are live in the local catalog, so removal
is a breaking change for whoever runs them. Migration is step 3 above rather
than a silent break. `mux doctor` should name the replacement.

**Not decided here.** Whether ADR-0014's resize endpoint is removed with PTY
or retained for a future non-PTY consumer. Flagged in step 5, deliberately
left open rather than answered by implication.
