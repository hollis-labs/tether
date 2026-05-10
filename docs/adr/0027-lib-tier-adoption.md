# ADR 0027 — Lib Tier Adoption for CLI Providers

**Status:** Accepted
**Date:** 2026-05-09
**Supersedes:** —
**Superseded by:** —

## Context

Agent Mux shipped its first `go-agent-sessions` adoption at a much earlier portfolio tier:

- `go-providers v0.5.0`
- `go-agent-sessions v0.1.0`
- `go-sandbox v0.1.0`
- `go-runner v0.1.0`

On 2026-05-08 and 2026-05-09 the shared Go portfolio moved again:

- `go-providers v0.10.0` removed all HTTP API providers.
- `go-providers v0.12.0` removed unused CLI adapters, keeping only Claude, Codex, and Opencode.
- `go-providers v0.13.0` added Claude `apiKeyHelper` support for bare-mode users.
- `go-agent-sessions v0.7.1` added the long-lived PTY runtime, `WorkspaceDir`, `AutoFireFirstTurn`, typed-event hooks, improved PID reporting, and structured `ExitError` propagation.
- `go-sandbox v0.2.0` added `Profile.AllowLoopback`.
- `go-runner v0.4.0` carries the structured exit-cause model consumed by the newer session layer.

Mux consumes only CLI-backed providers today. It has no current HTTP LLM consumers. That means the portfolio reshape does not require an HTTP migration in this repo, but it does make several app-local implementations redundant.

## Decision

Mux adopts the current CLI/session/sandbox tier:

- `github.com/hollis-labs/go-providers v0.13.0`
- `github.com/hollis-labs/go-agent-sessions v0.7.1`
- `github.com/hollis-labs/go-sandbox v0.2.0`
- `github.com/hollis-labs/go-runner v0.4.0`

Mux deletes its app-local long-lived Claude PTY runtime in `internal/provider/cli/claudecode/` and re-expresses that path as a normal `go-agent-sessions` adapter runtime with:

- `provider.NewClaudeAdapterPTY()`
- `agentsessions.NewFromAdapter(...Caps.PTY=true...)`
- `StartOptions.WorkspaceDir`
- `StartOptions.AutoFireFirstTurn`

Mux does not adopt `go-llm-contracts` or `go-embed-contracts` as first-class mux interfaces in this sprint. `go-llm-types` now appears as an implementation-side dependency pulled in by the upgraded shared CLI/session libraries, but mux still defers any direct API-provider surface shaped around the `go-llm-*` repos to the future sprint described in ADR 0029.

## Consequences

- The app-local PTY package is deleted instead of shimmed.
- Claude long-lived sessions now share the same adapter substrate as Claude Stream, Codex, and Opencode.
- Boot-dir planting is delegated to `go-providers` rather than owned piecemeal by mux.
- `workspace-plus-net` sessions explicitly opt into loopback under `go-sandbox`.
- Mux's quality gate now runs against Go 1.26.3 because `govulncheck` flagged standard-library issues in 1.26.2.

## Notes

This ADR intentionally skips the HTTP migration arc. The canonical portfolio reference for that work is `agent-workspaces/knowledge/portfolio/vendor-sdk-migration-guide.md`, but mux has no active call sites that need it today.
