# ADR 0039: boot-exec Stays Claude-TUI-Only; Codex/Opencode Direct Exec Deferred

**Status:** Accepted — 2026-05-15
**Context:** CW-20260515-0124
**Deciders:** Tether shared-launch closeout task CW-0118

## Context

`mux boot-exec <profile>` is a thin convenience path: it renders a boot
prompt, materializes the work root, compiles a launch plan through
`go-agent-launch`, and execs directly into the provider CLI so the operator
lands in a native TUI — bypassing Tether session creation, daemon attach, and
log replay.

Today `boot-exec` is Claude-only by construction. `internal/bootexec/claude.go`
`PrepareClaudeTUI` hard-errors when the launch plan's `ProviderBrand` is not
`claude`:

```
boot-exec currently supports claude launch profiles only (got provider %q)
```

It then builds the prepared command with `go-providers`
`NewClaudeAdapterPTY()`, which is specific to the Claude PTY runtime.

This is sometimes mistaken for a general provider gap. It is not. Codex and
Opencode are already first-class providers for **managed sessions**:

- the catalog carries `codex`/`opencode` provider records and launch profiles;
- the provider runtime matrix (ADR 0037) maps `codex + jsonrpc-stdio` and
  `opencode + subprocess` to their runtime factories;
- `internal/provider/cli/opencode` and `internal/provider/cli/claudestream`,
  plus the jsonrpc-stdio / subprocess transports, support those runtimes.

The only thing missing for Codex and Opencode is the **direct native
`boot-exec` convenience path** — not provider support generally. A managed
session (`mux launch` / `mux_session_create` + `mux_session_launch`) already
launches and attaches Codex and Opencode agents today.

## Decision

`boot-exec` stays **Claude-TUI-only for now**. Direct native `boot-exec` for
Codex and Opencode is **deferred to future provider work** — `later`, not
`never`, and not `now`.

Rationale:

- `boot-exec` is a thin Claude-PTY convenience wrapper. `PrepareClaudeTUI` is
  Claude-specific top to bottom (`ProviderBrand == "claude"` guard,
  `NewClaudeAdapterPTY()`), so extending it is real provider work, not a
  trivial branch.
- Codex and Opencode have different runtime transports. Codex app-server is
  JSON-RPC stdio; Opencode is subprocess-per-turn. Neither maps onto the
  Claude PTY exec shape. Each needs provider-specific runtime validation
  through the full `go-agent-launch` compile/prepare/plant flow before a
  direct-exec path can be trusted.
- **No capability is actually missing.** Managed sessions already cover Codex
  and Opencode end to end. `boot-exec` is a convenience for the native Claude
  TUI; its absence for other providers costs operators a convenience, not a
  capability. Operators who want a Codex or Opencode agent today use
  `mux launch` (or `mux_session_create` + `mux_session_launch`).
- The existing hard error in `bootexec/claude.go` is the correct behavior: it
  rejects an unsupported combination with a clear message instead of silently
  doing the wrong thing, consistent with the runtime-matrix philosophy in
  ADR 0037.

Future Codex and Opencode `boot-exec` support, when prioritized, should be
filed as separate provider-specific tasks (one per provider) so each runtime's
exec semantics can be designed and validated independently.

## Consequences

### Positive

- The boundary is explicit and documented: `boot-exec` = Claude TUI only;
  Codex/Opencode = managed sessions via `mux launch`.
- No half-built multi-provider `boot-exec` path is introduced before the
  Codex/Opencode exec semantics are designed.
- Operators are pointed at the supported path (`mux launch`) instead of
  hitting the hard error with no remedy.

### Negative

- Catalog launch support for Codex/Opencode does *not* imply `boot-exec`
  support, which can surprise operators — addressed by updating the
  `boot-exec` command help and operator docs.
- Operators who want a one-shot native Codex or Opencode TUI must use a
  managed session and attach, which is a slightly heavier flow than direct
  exec.

## Implementation Notes

- No change to `internal/bootexec/claude.go`. The `ProviderBrand != "claude"`
  hard error stays as the correct, documented behavior.
- The `mux boot-exec` command `Long` help states the Claude-only boundary and
  points to `mux boot` / `mux launch` for Codex and Opencode.
- `docs/catalog-launch-profiles.md`, `docs/shared-agent-launch-reference.md`,
  and `docs/shared-launch-adoption-guide.md` state plainly that `boot-exec` is
  Claude-TUI-only and that Codex/Opencode use managed sessions.
- Future provider-specific `boot-exec` work is tracked as separate tasks (see
  the Decision rationale): one for Codex app-server direct exec, one for
  Opencode subprocess direct exec.
