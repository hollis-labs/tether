# ADR 0030 — Long-Lived Lifecycle Integration

**Status:** Accepted
**Date:** 2026-05-11
**Supersedes:** Section "Claude Code PTY runtime" of ADR 0027 (PTY caps for `claude-code`).
**Superseded by:** —

## Context

ADR 0027 (lib tier adoption) shipped `claude-code` as a long-lived **PTY** runtime. The PTY path drove the Claude CLI's TUI through a raw terminal, used `AutoFireFirstTurn` to send `"Boot @./boot.md\n"` after spawn, and required workspace-plus-net sandbox augmentation to allow keychain access and Claude's `~/Library/Application Support/Claude` writes during TUI rendering. In live use it dies with `exit -1` shortly after the TUI renders — `cmd.Wait()` returns a non-`*ExitError` (typically SIGKILL from the sandbox blocking some path the TUI touches at startup). Nanite reproduced the same symptom in its Phase D scenario-2 test against the same lib stack, confirming it as a portfolio-wide PTY+TUI gap, not a Mux-specific bug.

Investigation on 2026-05-11 (recorded in `agent-workspaces/knowledge/portfolio/cli-agent-long-lived-modes.md`) revealed that the framing "PTY vs subprocess-per-turn" missed two vendor-documented headless long-lived modes that nobody had wired:

- **Claude streaming-stdio** (`claude -p --input-format stream-json --output-format stream-json --verbose`) — Anthropic's "Streaming Input Mode," a long-lived `claude -p` process that reads NDJSON user-messages on stdin, streams structured events on stdout, and retains in-process KV-cache across turns until stdin EOF. Empirically confirmed: turn 1 caches 73,350 tokens, turn 2 reads them back (same PID).
- **Codex app-server** (`codex app-server`) — the same engine that backs OpenAI's official VS Code extension. Long-lived process speaking JSON-RPC 2.0 over stdio. Multiple threads in memory until 30-min idle. The `pty_codex.go:22-27` comment `"Resume is interactive-only in Codex"` is stale as of codex 0.130.0; app-server is the supported long-lived path.

The portfolio lib tier landed support for both modes in:

- `go-providers v0.17.0` — `ClaudeAdapter.InputMode = "stream-json"` + `NewClaudeAdapterStreamingStdio()`; `CodexAdapter.Mode = "app-server"` + `NewCodexAdapterAppServer()`.
- `go-agent-sessions v0.8.0` — new runtime kinds `streamingStdioSession` and `jsonRpcStdioSession`; new `Capabilities.StreamingStdio` and `Capabilities.JsonRpcStdio` flags; mutual exclusion enforced in `NewFromAdapter`.
- `go-agent-sessions v0.9.0` — `Manager.JsonRpcCall(ctx, id, method, params)` so Manager-mediated consumers can drive JSON-RPC sessions without exposing raw Session references; opt-in `StartOptions.AutoPlantBootDir` for lib-tier BootDirSpec planting.
- `go-agent-sessions v0.9.1` — pre-seeds `lastSessionID` from `SessionIDPreset` at Start so `ProviderSessionID()` returns the preset immediately on long-lived runtimes.

This ADR records Mux's integration of the new lib tier for long-lived headless sessions.

## Decision

Mux switches the `claude-code` provider from PTY to **streaming-stdio**, adds **`codex-app-server`** as a new JSON-RPC stdio provider, and adds a lifecycle-aware **`SendTurn`** surface on the service layer.

### Lib pins

- `github.com/hollis-labs/go-providers v0.13.0` → **v0.17.0**
- `github.com/hollis-labs/go-agent-sessions v0.7.1` → **v0.9.1**

### Claude provider — streaming-stdio

`newClaudeCodeRuntime` (`internal/app/service.go`) builds the runtime from `gop.NewClaudeAdapterStreamingStdio()` with `Capabilities.StreamingStdio = true, ProviderSessionID = true, BinaryRequired = true`. `ApiKeyHelperPath` is wired the same way as before (planted `.claude/settings.json` carries the `apiKeyHelper` field; bare and non-bare both honor it).

The PTY-specific `AutoFireFirstTurn` block in `LaunchSession` is removed. The boot prompt rides via the provider's `BootDirSpec` planted `CLAUDE.md` + `--append-system-prompt-file` (bare mode) — both already in upstream go-providers. First user turn is whatever the caller delivers via `SendTurn` (or raw `SendInput` for callers that own framing).

`claude-code` is added to `providerHasSessionIDContinuity` so the `--resume <id>` chaining flow (`SessionIDPreset` + `OnSessionID` callback persisting to `store.SetClaudeSessionID`) applies to streaming-stdio sessions for cold-start recovery.

### Codex provider — app-server (JSON-RPC stdio)

`newCodexAppServerRuntime` (`internal/app/service.go`) builds from `gop.NewCodexAdapterAppServer()` with `Capabilities.JsonRpcStdio = true, BinaryRequired = true`. The runtime registers under provider id `codex-app-server` (distinct from the legacy `codex-cli` turn-based entry, which stays alongside as an interim substrate path).

Codex protocol bootstrap (`initialize` + `thread/start`) runs lazily inside `SendTurn` on the first call for a session; the resulting thread id is cached on the service via a `sync.Map` keyed by Mux session id. Subsequent `SendTurn` calls for the same session reuse the cached thread id directly to `turn/start`. JSON-RPC notifications from codex (`turn/started`, `item/agentMessage/delta`, `item/completed`, `turn/completed`, etc.) flow to the attach Fanout automatically via the lib's `jsonRpcStdioSession` reader loop — no Mux-side mapper required for the v005-05 wireup. A typed-event mapper at `internal/provider/cli/codex_appserver/events.go` can be added in a follow-up sprint if attach consumers want Mux-typed events instead of raw JSON frames.

### Service-layer `SendTurn`

New method `Service.SendTurn(ctx, sessionID, text)` dispatches per session caps:

- `StreamingStdio` → frames `{"type":"user","message":{"role":"user","content":<text>}}` + `\n` via `frameUserMessage`, writes to `Manager.SendInput`.
- `JsonRpcStdio` → lazy `initialize` + `thread/start` (cache thread id), then `turn/start` via `Manager.JsonRpcCall`.
- Otherwise (PTY or unknown) → raw `Manager.SendInput([]byte(text))` for back-compat.

`SendInput` stays — additive, kept for callers that own their own framing.

Wired surfaces: `POST /sessions/{id}/turn` (HTTP, JSON body `{"text": "..."}`); `mux_session_send_turn` (MCP); `mux sessions turn <id> <text>` (CLI). The legacy `SendInput`-shaped HTTP / MCP / CLI surfaces are unchanged.

### Capability DTO surface

`CapabilitiesDTO` (`internal/api/types.go`) gains `StreamingStdio bool` and `JSONRPCStdio bool` fields, propagated in `RuntimeHealthResponse` (HTTP) and `mux_session_health` (MCP). Without these, external consumers couldn't tell from health endpoints whether a session was in PTY, streaming-stdio, or JSON-RPC stdio mode.

### What stays

- `internal/provider/cli/claudestream/` package and its `plantBootDir` helper remain. The original sprint plan called for full deletion, but `go-agent-sessions` v0.8.0 ships without BootDirSpec planting (verified: zero references in any tagged version). v0.9.0 introduced opt-in lib-tier planting via `StartOptions.AutoPlantBootDir`, agreed for adoption in a follow-up sprint after Mux + Nanite + Clockwork coordinate the migration together. For v005-05, `claudestream.NewWithAdapter` continues to plant boot dirs and clean them up on session terminal state. The `bootDirSession` wrapper gained an explicit `ProviderSessionID()` forwarder because Go interface method promotion doesn't carry optional interfaces (`SessionIDer`) through an embedded interface declaration.
- The legacy `claude-stream` (turn-based subprocess-per-turn) and `cli-goprovider`-typed catalog entries (`codex-cli`, etc.) stay alongside the new providers. They're not the target steady-state but remain useful during the transition.

## Consequences

### Positive

- Claude `exit -1` symptom dies. No more TUI to render, no more raw terminal, no more keychain ceremony in the hot path. Streaming-stdio reads NDJSON and exits cleanly on stdin EOF.
- Mux gets KV-cache continuity on Claude — turn N+1 reads tokens cached on turn N from the same PID. Per-turn cost drops correspondingly.
- Codex gets first-class app-server support via the same engine the OpenAI VS Code extension uses. Multiple threads in memory; thread/start/resume/fork/list all reachable through `Manager.JsonRpcCall`.
- `SendTurn` becomes the recommended way to deliver user turns. Consumers stop hand-rolling NDJSON envelopes or JSON-RPC framing; per-mode discipline lives in one method on the service.
- Capability DTO surface stays honest: external observers can tell what mode a session is in.

### Neutral / known limitations

- `claudestream` package didn't disappear in this sprint. Follow-up captured in Vanta (`decisions_go_agent_sessions_v009_absorb_bootdirspec_planting`); ships in the next-session sprint after lib-tier planting is adopted.
- Codex `initialize` / `thread/start` / `turn/start` params are currently `{}` / `{threadId, input}`. Real codex `app-server` may require richer params (`clientInfo`, `clientCapabilities`, etc.). The live smoke (see Smoke section) will surface the exact required shape; refinements land in a small follow-up if needed.
- The PTY-conditional first-turn payload (`"Boot @./boot.md\n"`) is gone. Callers that relied on that implicit kickoff now drive the first turn explicitly via `SendTurn` (or `SendInput`). No Mux-internal callers were doing this; the change is at the consumer boundary.

### Negative

- One new lib gap was filed and resolved during the sprint (`Manager.JsonRpcCall` in v0.9.0; `SessionIDPreset` pre-seeding in v0.9.1). Both reflected sub-boot prompt assumptions that didn't match v0.8.0 lib reality. The portfolio's parallel lib agent absorbed the fixes immediately; the Mux side bumped pins twice mid-sprint.

## Smoke acceptance

The live smoke (recorded separately in `agent-workspaces/execution/agent-mux/v005-05-long-lived-integration/2026-05-11/smoke-results.md`) verifies two load-bearing invariants:

- **Claude:** turn 2's `usage` event carries `cache_read_input_tokens > 0` — proves single PID + retained in-process state across turns.
- **Codex:** two `SendTurn` calls on the same session return the same `threadId` via cached state and via a follow-up `thread/list` `JsonRpcCall` — proves the thread didn't reset.

Each smoke is run once via CLI (`mux sessions turn`), once via MCP (`mux_session_send_turn`), and once via HTTP (`POST /sessions/{id}/turn`).

## Files touched

| Path | Change |
|---|---|
| `go.mod` / `go.sum` | Pin bumps: go-providers v0.17.0, go-agent-sessions v0.9.1 |
| `internal/app/service.go` | `newClaudeCodeRuntime` → streaming-stdio; `newCodexAppServerRuntime` (new); `Service.SendTurn` + `frameUserMessage` + `sendTurnJSONRPC` (new); `codexThreads sync.Map` on Service; `claude-code` added to `providerHasSessionIDContinuity`; PTY-conditional first-turn block deleted |
| `internal/api/types.go` | `CapabilitiesDTO` gains `StreamingStdio` + `JSONRPCStdio`; `LaunchService` gains `SendTurn` |
| `internal/api/sessions.go` | Router gains `case "turn"`; `RuntimeHealthResponse` populates the two new cap fields |
| `internal/api/input.go` | `handleSendTurn` + `SendTurnRequest` (new) |
| `internal/client/client.go` | `Client.SendTurn` (new) |
| `cmd/mux/sessions.go` | `sessionsTurnCmd` (new) |
| `cmd/mux/daemon.go` | `serviceAdapter.SendTurn` passthrough |
| `internal/mcpadapter/sessions.go` | `mux_session_send_turn` tool + handler; health response populates new cap fields |
| `internal/provider/cli/claudestream/cliadapter.go` | `bootDirSession.ProviderSessionID()` forwarder for optional-interface assertion |
| `examples/catalog/providers/claude-code.yaml` | `bootstrap.mode` → `streaming-stdio` + comment |
| `examples/catalog/providers/codex-app-server.yaml` | NEW |
| `internal/app/service_lifecycle_caps_test.go` | NEW — 3 cap + framing tests |
| `internal/app/service_codex_app_server_test.go` | NEW — compliance harness |
| `internal/api/handlers_test.go` / `internal/client/client_test.go` | `SendTurn` no-op on test fakes |
