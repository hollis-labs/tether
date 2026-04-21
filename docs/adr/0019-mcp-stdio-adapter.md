# ADR 0019 — MCP stdio Adapter

**Status:** accepted  
**Date:** 2026-04-21  
**Supersedes:** —  
**Related:** ADR 0002 (daemon transport), ADR 0009 (local API create/launch split), ADR 0018 (broker envelope types)

---

## Context

Agent-mux exposes a local HTTP API over a Unix-domain socket (ADR 0002). Clients
access it via the `go-agentmux-client` library or raw HTTP. As the portfolio grows,
LLM-based tools (Claude Code, Codex, Kiro, Hadron blueprints, custom agents) need
to call agent-mux capabilities directly from tool calls — without writing HTTP
client code or depending on the Go client library.

The Model Context Protocol (MCP) stdio transport is the natural interop layer: an
agent host launches `mux mcp` as a subprocess, connects stdin/stdout, and receives
a tool catalog it can call. This is already the pattern used by Hadron and
Clockwork-Manifold in the portfolio.

## Decision

Add `internal/mcpadapter` — a thin wrapper that exposes `app.Service` as MCP
tools over stdio — and wire it as `mux mcp` in the CLI.

**Key design choices:**

1. **In-process, not over HTTP.** The adapter wraps `*app.Service` directly rather
   than talking to the running daemon over UDS. This avoids a network round-trip,
   simplifies auth (no socket path resolution), and makes the adapter easy to test.
   The trade-off: `mux mcp` spins up its own service instance and SQLite connection.

2. **Same library, same version as portfolio peers.** `github.com/mark3labs/mcp-go
   v0.47.0` — matches clockwork-manifold, newer than hadron/vanta-conduit (v0.44.1).

3. **Token + scope gating for mutating tools.** Read-only tools (health, catalog
   reads, session reads) are ungated. Mutating tools require a bearer token and the
   specific scope string:
   - `session.write` — create, launch, stop, wait, input, resize, resume
   - `message.write` — send, consume, cancel

   Token and scopes are passed via `--token`/`--scopes` flags or
   `AGENT_MUX_MCP_TOKEN` / `AGENT_MUX_MCP_SCOPES` environment variables.

4. **Tool naming convention: `mux_<group>_<verb>`.** Consistent with hadron_* and
   clockwork_* prefixes in the portfolio. Groups: `health`, `catalog`, `session`,
   `logical_agent`, `message`, `boot`.

5. **SSE streaming (attach output) is out of scope.** MCP tool results are
   request/response; streaming PTY output requires a different transport. Deferred.

## Tool inventory (v0.1.0)

| Tool | Scope | Description |
|---|---|---|
| `mux_health` | — | Adapter health + catalog summary |
| `mux_catalog_list_projects` | — | List catalog projects |
| `mux_catalog_list_agents` | — | List catalog agent profiles |
| `mux_catalog_list_providers` | — | List catalog providers |
| `mux_catalog_list_launches` | — | List catalog launch profiles |
| `mux_catalog_list_boot_profiles` | — | List boot-profile YAMLs |
| `mux_session_list` | — | List sessions (filter + paginate) |
| `mux_session_get` | — | Get session by ID |
| `mux_session_create` | session.write | Create session from launch profile |
| `mux_session_launch` | session.write | Launch a created session |
| `mux_session_stop` | session.write | Stop a running session |
| `mux_session_wait` | — | Block until session exits |
| `mux_session_send_input` | session.write | Send PTY input |
| `mux_session_resize` | session.write | Resize PTY |
| `mux_logical_agent_list` | — | List logical agents |
| `mux_logical_agent_resume` | session.write | Resume agent from checkpoint |
| `mux_message_send` | message.write | Send message envelope |
| `mux_message_get` | — | Get message by ID |
| `mux_message_inbox` | — | List inbox for recipient |
| `mux_message_thread` | — | List thread by thread_id |
| `mux_message_consume` | message.write | Consume message |
| `mux_message_cancel` | message.write | Cancel message |
| `mux_boot_generate` | — | Generate boot prompt from profile |

## Consequences

**Good:**
- Any MCP-capable client (Claude Code, Cursor, custom agents) can call agent-mux
  session lifecycle and messaging without HTTP client code.
- The boot prompt generator (`mux generate-boot`) is now callable as a tool,
  enabling agents to self-boot or boot peers.
- No daemon required for read-only catalog/session queries — useful in scripting
  contexts where the daemon may not be running.

**Trade-offs:**
- `mux mcp` opens a second SQLite connection. For write-heavy workloads, concurrent
  access with a running daemon is possible. SQLite WAL mode (already enabled) handles
  this; writers serialize at the DB level. If contention becomes an issue, a
  proxy-over-UDS mode (routing through the daemon's HTTP API) is the upgrade path.
- No streaming: agents that want to follow PTY output in real time must poll
  `mux_session_get` or use the existing `go-agentmux-client` SSE subscription.

## Upgrade path

If the in-process model proves insufficient (e.g., multiple concurrent MCP sessions
creating lock contention), replace `app.Service` with a thin HTTP client over UDS so
all writes go through the running daemon. The tool interface is unchanged; only the
adapter's internal transport changes.
