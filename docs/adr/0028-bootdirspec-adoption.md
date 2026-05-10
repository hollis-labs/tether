# ADR 0028 — Adopt `BootDirSpec` for Provider Boot Layouts

**Status:** Accepted
**Date:** 2026-05-09
**Supersedes:** —
**Superseded by:** —

## Context

Mux had accumulated provider-specific boot layout logic in multiple places:

- Claude PTY boot files in the deleted `internal/provider/cli/claudecode/`
- direct-launch TUI boot helpers in `internal/tui/externshell`
- ad hoc `bootstrap.mode: agents_md` handling for Codex-style flows

At the same time `go-providers` now publishes boot-dir conventions directly on the adapters through `BootDirProvider` and `BootDirSpec`.

Mux needs one boot mechanism that:

- works for Claude PTY, Claude Stream, Codex, and Opencode
- lets the provider library own the planted-file convention
- keeps provider-specific cwd/env/project-dir behavior out of mux business logic

## Decision

Mux adopts `adapter.BootDirSpec()` as the authoritative boot-dir contract for CLI providers.

For adapter-backed sessions, mux now:

1. Creates a per-session boot dir.
2. Iterates `BootDirSpec.PlantedFiles`.
3. Renders files with `provider.PlantContext`.
4. Applies `EnvAmendments`.
5. Uses `SpawnWorkdir(...)` to choose the child cwd.
6. Appends the rendered `ProjectDirArg` to spawn args when appropriate.

For Claude PTY sessions the boot prompt no longer rides stdin as the primary system-context mechanism. Instead:

- `CLAUDE.md` and `boot.md` are planted by the spec.
- `AutoFireFirstTurn` delivers `Boot @./boot.md`.

Claude's `apiKeyHelper` support also rides through this decision: mux sets `ClaudeAdapter.ApiKeyHelperPath` before the spec renders `.claude/settings.json`, so the planted settings file can carry the helper path when configured.

## Consequences

- `bootstrap.mode: agents_md` becomes effectively a compatibility no-op for mux's adapter path. The file-planting behavior is now derived from the provider adapter, not from catalog-local branching.
- Project-dir access flags such as `--add-dir` and `--cd` are library-owned data, not mux-owned string constants.
- Provider-specific tempdir cleanup now happens at the session wrapper layer after `Wait()` or `Stop()`.

## Deferred

- Claudestream remains the same adapter family as Claude Stream JSON subprocesses, but it now gets its boot layout from the same Claude `BootDirSpec`.
- TUI direct-boot remains separate from daemon launches and is not the contract-defining path for boot layout anymore.
