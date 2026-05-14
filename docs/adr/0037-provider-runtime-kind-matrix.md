# ADR 0037: Provider Runtime Kind Matrix

**Status:** Accepted — 2026-05-14
**Context:** SP-20260514-0001
**Deciders:** Tether provider-runtime sprint

## Context

The catalog historically used provider IDs such as `claude-code`,
`codex-app-server`, `codex-cli`, and `opencode` as both product labels and
runtime selectors. That made it hard to add another Claude runtime without
renaming or changing the behavior of `claude-code`, which is already the
managed streaming-stdio default.

Mux needs both managed sessions and user-driven TUI sessions:

- managed Claude streaming via NDJSON `streaming-stdio`;
- interactive Claude TUI via a PTY with attach/detach/resize;
- Codex app-server via JSON-RPC stdio;
- Codex, Claude, and Opencode subprocess-per-turn adapters.

## Decision

Catalog provider records carry two separate selectors:

- `provider`: product/adapter brand, for example `claude`, `codex`, or
  `opencode`;
- `runtime_kind`: lifecycle/transport, one of `pty`, `streaming-stdio`,
  `jsonrpc-stdio`, `subprocess`, or `api`.

Launch profiles continue to reference concrete provider profile IDs. The
provider profile ID remains the stable operator-facing handle, while the
resolver matrix maps `provider + runtime_kind` to the runtime factory.

Compatibility rules:

- existing catalogs without `provider` derive the brand from `adapter`,
  `type`, or the legacy ID prefix;
- existing catalogs without `runtime_kind` derive it from `bootstrap.mode`
  for `streaming-stdio` and `jsonrpc-stdio`, from `type: api` for API
  providers, and otherwise default to `subprocess`;
- `claude-code` remains `claude + streaming-stdio`;
- `codex-app-server` remains `codex + jsonrpc-stdio`;
- `codex-cli`, `claude-goprovider`, and `opencode` remain subprocess
  runtimes.

The matrix intentionally rejects unsupported combinations with a clear error
instead of silently falling back. Future provider brands can be added by
extending the matrix after their runtime semantics are known.

## Consequences

`claude-pty` can be added as a new provider profile without changing
`claude-code`. Its runtime uses `go-providers` `NewClaudeAdapterPTY` with
`go-agent-sessions` PTY capabilities: `PTY`, `Resize`, `ProviderSessionID`,
and `BinaryRequired`. Launching still flows through catalog resolution,
AutoPlantBootDir, boot-dir/config injection, session persistence, and
`mux sessions attach` detach/resize semantics.

Docs and examples should distinguish launch profile IDs (`torque-claude`,
`torque-claude-tui`) from boot profile IDs (`torque.engineer.main`,
`torque.engineer.tui`) so operators know whether they are starting from a
catalog launch or a boot-profile-rendered launch.
