# ADR 0034 — ACP Surface (Agent Client Protocol Server)

**Status:** Accepted
**Date:** 2026-05-12
**Supersedes:** —
**Superseded by:** —

## Context

Mux speaks three consumer surfaces today: a CLI, an MCP stdio adapter (`mux mcp`), and an HTTP API on UDS. Each routes to the same service-layer primitives. With editors increasingly standardizing on the [Agent Client Protocol](https://agentclientprotocol.com) (ACP) — Zed natively, JetBrains via `acp.json`, Avante.nvim and CodeCompanion.nvim via plugin — adding ACP as a fourth surface lets editors drive Mux sessions without per-editor integration code.

Two questions had to settle before implementation:

1. **Expose only, or also consume?** Mux can act as an ACP *server* (editors connect to Mux as their AI agent) or an ACP *client* (Mux connects out to other ACP-speaking agents and aggregates them — analogous to `mux mcp --proxy`).
2. **Method coverage and capability shape.** ACP defines ~10 inbound + ~8 outbound methods plus `session/update` notifications. Not all are required for an MVP; some require translating CLI-agent tool-use semantics into ACP shape, which is a meaningful layer of engineering.

## Decision

### 1. Expose only — Mux as ACP server, not client

The `mux acp` subcommand exposes the service layer to editor clients. **Consuming external ACP agents is deferred to post-beta plugin-sdk work.** Mux's role is a session control plane; aggregating external agents as if they were Mux sessions blurs that role and belongs in plugin-sdk territory.

### 2. New `internal/acpadapter/` package designed for portfolio extraction

Given no Go ACP library exists upstream, we wrote the server-side scaffolding from scratch (similar shape to mark3labs/mcp-go for MCP). The package is laid out for future extraction to a portfolio `go-acp` library:

- No imports of mux-internal types from inside the package.
- `acpadapter.Service` interface is the host integration seam.
- Stdio framing (newline-delimited JSON-RPC 2.0, no Content-Length), bidirectional dispatcher, request-response correlator, and auth middleware each in dedicated files with no host-specific code.

Extraction itself is captured as `followup_portfolio_go_acp_extraction` in Vanta. Mux-specific glue (`internal/acpsvc/` Service implementation, `cmd/mux/acp.go` subcommand, boot profile resolution) lives outside `internal/acpadapter/`.

### 3. MVP method coverage (locked during readiness pass)

| Direction | Method | MVP | Notes |
|---|---|---|---|
| C→A | `initialize` | ✓ | Negotiates protocol version + capabilities |
| C→A | `authenticate` | ✓ | Bearer token via `{"token":"..."}` body |
| C→A | `session/new` | ✓ | Routes to daemon `LaunchSession`; ignores editor `mcpServers` |
| C→A | `session/prompt` | ✓ | Long-lived; streams via `session/update` notifications |
| C→A | `session/cancel` (notif) | ✓ | Best-effort — see §5 |
| C→A | `session/close` | ✓ | Routes to daemon `Stop` |
| C→A | `session/resume` | ✓ | Multi-client attach pathway |
| C→A | `session/load` | ✗ defer | Requires history-replay materialization |
| C→A | `session/list` | ✗ defer | Use `mux_session_list` MCP tool today |
| C→A | `session/set_mode` / `set_config_option` | ✗ decline | Underlying agents lack ACP-mode mapping |
| A→C | `session/update` (notif) | ✓ | `agent_message_chunk` variant only MVP |
| A→C | `session/request_permission`, `fs/*`, `terminal/*` | ✗ decline MVP | See §5 |

### 4. Capability advertisement (initialize response)

| Capability | Value | Reason |
|---|---|---|
| `loadSession` | false | Resume covers multi-client; load adds history-replay complexity |
| `promptCapabilities.image/audio/embeddedContext` | false | Text-only MVP |
| `mcpCapabilities.http/sse` | false | Boot-profile MCPs apply, not editor-supplied |
| `sessionCapabilities.resume` | true | Required for multi-client per §1 lock |
| `sessionCapabilities.close` | true | Cheap; clean lifecycle |
| `sessionCapabilities.list` | false | `mux_session_list` over MCP suffices |
| `authMethods` | `[token]` | Mirrors `mux mcp` token+scope model |

### 5. Tool-call / fs / terminal proxying — declined MVP

ACP defines `session/request_permission` (agent → client) for risky tool calls, plus `fs/*` and `terminal/*` for the agent to drive the editor's filesystem and terminal. These imply the agent translates its underlying tool-use events into ACP-shaped `tool_call` updates and reaches back through the editor for execution.

Mux's underlying CLI agents (Claude Code, Codex) **handle filesystem and terminal access via their own internal tool plumbing** — they don't ask Mux to ask the editor. Translating those into ACP tool-call notifications + permission-prompt cycles is a meaningful engineering layer that doesn't block first-turn dogfood.

**Trade-off:** The editor's ACP-aware UI for tool calls / permission prompts / file diffs / terminal output doesn't fire when talking to Mux MVP. Streaming text + completion stop reasons work end-to-end; tool-execution observability is invisible at the ACP layer.

This is captured as a portfolio-wide follow-up — the translation layer benefits Nanite + Clockwork too if they ever expose ACP. The bidirectional dispatcher (`Call()` for outbound requests + response correlation) is built in MVP so future tool-call proxying drops in without framing changes.

### 6. Cancellation is best-effort

`session/cancel` propagates to `acpadapter.Service.CancelTurn`, which marks the in-flight turn so the eventual claudestream `done` event maps to `StopReasonCancelled`. **The underlying agent is not interrupted** — there is no daemon interrupt primitive yet. The editor sees the right wire-level stop reason; the agent finishes generating naturally.

Captured as `followup_acp_cancel_underlying_interrupt` for v005-10 hardening.

### 7. Multi-client via `session/resume`

ACP doesn't define a multi-client protocol semantic — each ACP connection is its own JSON-RPC channel. To let two editors share a session, the second editor calls `session/resume` with the known sessionId. Mux verifies the session exists on the daemon, starts a per-connection attach goroutine, and streams events from the existing daemon-side session.

Each ACP connection tracks its own owned-sessions set for clean shutdown; the daemon-side session lifetime is independent of either editor disconnecting.

## Consequences

### Positive

- Editors can drive Mux sessions natively (Zed first; JetBrains/Neovim via shipped configs).
- `mux acp` is symmetric to `mux mcp` — same auth model, same `--token`/`--scopes` flags, same scope vocabulary (`session.write`).
- Package extraction-ready for future `go-acp` portfolio lib.
- ACP and MCP surfaces now both route session-mutating ops through the daemon (see ADR 0035), eliminating the in-process split-brain.

### Negative / deferred

- Tool-call observability gap: ACP clients don't see Claude/Codex tool calls, file edits, or terminal output as ACP-level events.
- Cancellation is wire-level only; underlying agent continues until natural end.
- `session/list` and `session/load` deferred (no protocol-clean way for editors to enumerate or replay sessions through Mux yet).
- Editor-supplied `mcpServers` on `session/new` are ignored — boot-profile MCPs apply.
- `mux_session_health` parity is partial — see ADR 0035.

### Operational

- New subcommand: `mux acp --agent <launch_id>` (REQUIRED flag picks the launch profile).
- New env vars: `AGENT_MUX_ACP_TOKEN`, `AGENT_MUX_ACP_SCOPES`.
- Editor configs documented in `docs/acp.md` (Zed, JetBrains, Neovim).
- ACP MVP method set is stable per this ADR; expansion (load/list/tool-call translation) requires a superseding ADR.

## References

- Spec: https://agentclientprotocol.com (entire `/protocol/*` set + announcements)
- Method-mapping doc: `agent-workspaces/execution/agent-mux/v005-09-acp-surface/2026-05-12/acp-method-mapping.md`
- Companion: ADR 0035 (mcpadapter daemon-client routing)
- Vanta: `mux_beta_push_roadmap_may_2026`, `followup_portfolio_go_acp_extraction`, `followup_acp_cancel_underlying_interrupt`, `followup_acp_session_cwd_workspace_override`
