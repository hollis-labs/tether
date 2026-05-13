---
title: Agent Mux — MCP Proxy / Tool Aggregator
date: 2026-04-21
status: draft
related:
  - docs/adr/0019-mcp-stdio-adapter.md
  - docs/sprints/v005-01-mcp-stdio-adapter.md
  - go-toolbroker (github.com/hollis-labs/go-toolbroker)
---

# Agent Mux — MCP Proxy / Tool Aggregator

## Problem

A developer using Claude Code, Cursor, or a custom agent typically has several
MCP servers configured: one for Hadron, one for Clockwork, one for Vanta, one
for a filesystem tool, etc. The agent sees a flat bag of 80+ tools. Context
fills up. There's no logging of what got called and with what args. There's no
way to apply consistent policy (auth, PII, rate limits) across all of them.

**The agent also needs to know about `mux` itself** — and right now that's yet
another server entry in the client config.

**One process that the agent talks to, which does everything else:** that's the
pitch.

## Vision (three horizons)

### Horizon 1 — Aggregation (buildable now)

`mux mcp` already exposes mux-native tools. Extend it to:

1. Read upstream MCP server definitions from the catalog.
2. On startup, launch each upstream server as a subprocess (stdio transport) or
   connect to it (SSE/HTTP transport).
3. Call `ListTools` on each, collect `ToolDefinition` records, and re-register
   them on the outward-facing `MCPServer` instance.
4. When a proxied tool is called, forward the call to the originating upstream
   server and return its result.

From the LLM client's perspective: one MCP server, all tools available. Agent
config goes from 6 server entries to 1.

**Go-toolbroker integration point:** `LocalBroker.RegisterTools()` already
accepts `[]ToolDefinition` with a `Server` field. The proxy can populate the
broker from upstream tool lists, giving intent-aware selection on top of
aggregation.

### Horizon 2 — Observability (medium-term)

Every tool call passing through the proxy gets a structured event:

```
{ session_id, tool_name, server, args_schema_fingerprint, duration_ms, ok, error? }
```

Published to the existing event bus (`internal/events`). Surfaces in:
- The TUI's event panel (live tool call feed per session)
- `/events` SSE (for external dashboards or alerts)
- The SQLite `events` table (historical query via `mux events list`)

This is zero-cost to build once the proxy scaffolding exists — just emit an
event in the proxy's `CallTool` path.

### Horizon 3 — Middleware pipeline (long-term)

A `ToolCallMiddleware` interface sits between the proxy receiver and the
upstream forwarder:

```go
type ToolCallMiddleware interface {
    Handle(ctx context.Context, call ToolCall, next ToolCallHandler) (ToolResult, error)
}
```

Middleware chain is configured per-server or globally. Initial candidates:

| Middleware | What it does |
|---|---|
| `LoggingMiddleware` | Structured event emission (H2 observability) |
| `PIIRedactionMiddleware` | Detect and redact PII patterns in args before forwarding |
| `RateLimitMiddleware` | Per-tool or per-server call rate cap |
| `ArgValidationMiddleware` | Enforce additional schema constraints beyond tool schema |
| `PromptInjectionMiddleware` | Detect injection patterns in string args |
| `AuditMiddleware` | Append-only audit log for compliance |

The pattern is identical to `go-toolbroker`'s `Enricher` interface — the right
place for this to eventually live is a `go-mcp-middleware` shared package.

---

## Architecture

```
LLM client (Claude / Cursor / custom agent)
         │  MCP stdio / SSE
         ▼
  ┌─────────────────────────────────────────────┐
  │            mux mcp (proxy mode)              │
  │                                             │
  │  ToolRegistry ◄── ListTools (startup)        │
  │       │                                     │
  │  ProxyRouter ── middleware chain             │
  │       │                                     │
  │  native tools   upstream forwarder           │
  │  (sessions,     (per-server client pool)     │
  │   catalog,                                  │
  │   messages,     ┌──────────┐  ┌──────────┐  │
  │   boot)         │ hadron   │  │clockwork │  │
  └─────────────────┤  client  ├──┤  client  ├──┘
                    └──────────┘  └──────────┘
                         │              │
                    hadrond mcp   clockwork mcp
                    (subprocess)  (subprocess/SSE)
```

### Key types

```go
// MCPServerEntry is one upstream MCP server definition in the catalog.
type MCPServerEntry struct {
    ID          string            // e.g. "hadron", "clockwork"
    Transport   string            // "stdio" | "sse"
    // Stdio transport fields:
    Command     string            // e.g. "/path/to/hadrond"
    Args        []string          // e.g. ["mcp"]
    Env         map[string]string // additional env vars
    // SSE transport fields:
    URL         string            // e.g. "http://localhost:8095/mcp/sse"
    // Shared:
    Token       string            // bearer token (or env var reference like $HADRON_TOKEN)
    Scopes      []string
    Enabled     bool              // default true; false = skip at startup
    Tags        []string          // capability tags forwarded to tool broker
}

// ToolRegistry holds the merged tool set: native + all upstreams.
type ToolRegistry struct {
    tools   map[string]registeredTool   // name → {definition, client}
    mu      sync.RWMutex
}

// ProxyRouter routes an incoming CallTool to the correct upstream client.
type ProxyRouter struct {
    registry   *ToolRegistry
    middleware []ToolCallMiddleware
}
```

---

## Catalog schema (proposed extension)

MCP server definitions live alongside existing catalog entries:

```
~/.agent-mux/catalog/
  mcp-servers/
    hadron.yaml
    clockwork.yaml
    vanta.yaml
    filesystem.yaml
```

Example `hadron.yaml`:

```yaml
id: hadron
transport: stdio
command: /path/to/hadrond
args: [mcp]
env:
  HADRON_TOKEN: "${HADRON_TOKEN}"   # env var expansion at load time
scopes: [run.write, schedule.write]
tags: [automation, ci, blueprints]
enabled: true
```

Example `vanta.yaml` (SSE transport):

```yaml
id: vanta
transport: sse
url: "http://localhost:8090/mcp/sse"
env:
  VANTA_TOKEN: "${VANTA_TOKEN}"
tags: [memory, knowledge, context]
enabled: true
```

`mux mcp --proxy` (or `mux mcp` with proxy enabled in global config) reads all
enabled entries, starts clients, and merges their tool lists.

---

## Tool naming and collision handling

Upstream tools keep their original names (`hadron_run_enqueue`,
`clockwork_task_create`, etc.). Portfolio convention already uses the
`<server>_<group>_<verb>` prefix pattern — collisions are structurally
unlikely.

If a collision does occur (two upstreams define the same name), the proxy:
1. Logs a warning at startup.
2. Keeps both, disambiguating with `<server_id>__<tool_name>` for the
   conflicting entry.

---

## Tool broker integration

The existing `go-toolbroker` `LocalBroker` is the intent-selection layer.
After aggregation, the proxy can register all collected tools into the broker:

```go
broker.RegisterTools(registry.AllDefinitions())
```

This gives the LLM client (or an orchestrator) the ability to ask:
"I need tools for CI automation" → broker returns `hadron_*` tools scored
by intent, rather than dumping 200 tool names into the context.

The proxy can expose a `mux_select_tools` meta-tool that wraps
`broker.SelectTools()` — the LLM calls it to get a focused subset before
doing real work.

---

## Phased implementation plan

### Phase 1 — Catalog + client pool (one sprint)

**Goal:** `mux mcp` launches upstream servers and exposes their tools alongside
native tools. No middleware, no broker integration yet.

Tasks:
- `internal/config/mcp_server.go` — `MCPServerEntry` type, catalog loader
  (`<catalog>/mcp-servers/*.yaml`)
- `internal/mcpadapter/registry.go` — `ToolRegistry` with mutex
- `internal/mcpadapter/client_pool.go` — startup: spawn stdio clients, connect
  SSE clients; `ListTools` on each; populate registry
- `internal/mcpadapter/proxy.go` — `ProxyRouter.Handle()` — receive
  `CallToolRequest`, look up entry, forward to upstream client, return result
- `cmd/mux/mcp.go` — add `--proxy` flag (or detect `mcp-servers/` dir and
  auto-enable); wire `clientPool` + `proxyRouter`
- `internal/mcpadapter/catalog.go` — `mux_catalog_list_mcp_servers` tool
  (list configured upstream servers)
- ADR 0020

**Acceptance:**
- `mux mcp --proxy` starts, spawns `hadrond mcp` as a subprocess, registers
  `hadron_*` tools, and correctly forwards a call to it
- Native mux tools still work alongside proxied tools
- Upstream process dies → proxy returns error for that server's tools, native
  tools unaffected

### Phase 2 — Observability (follow-on sprint)

**Goal:** every proxied tool call emits a structured event.

Tasks:
- `LoggingMiddleware` — emits `tool_call_start` / `tool_call_end` events to
  `internal/events` Bus
- `mux_events_tool_calls` query tool — list recent tool call events
- TUI: tool call feed in event panel
- ADR (amend 0020 or 0021)

### Phase 3 — Middleware pipeline (future sprint)

**Goal:** pluggable transforms on call path.

Tasks:
- `ToolCallMiddleware` interface in `internal/mcpadapter/middleware.go`
- `PIIRedactionMiddleware` (regex-based, configurable patterns in global config)
- `RateLimitMiddleware` (token bucket per tool or per server)
- `go-mcp-middleware` extraction (when second consumer exists — likely Nanite)

---

## Decisions locked for Phase 1

| Decision | Rationale |
|---|---|
| Transport: stdio subprocess (primary) | Zero daemon dependency; same pattern as how client apps use `mux mcp` itself |
| Transport: SSE (secondary) | Needed for long-running servers (Vanta, Clockwork) that are already started |
| Catalog in `mcp-servers/*.yaml` | Consistent with existing `providers/`, `agents/` directories |
| Tool names unchanged | Portfolio prefix convention prevents collisions; transparency for users |
| `--proxy` flag, not always-on | Proxy startup has latency (subprocess spawn + ListTools RTT); opt-in for now |
| Native tools always registered | Even in proxy mode, `mux_*` tools are always available |

## Open questions

1. **Token/scope propagation** — should upstream tokens be stored in the catalog
   (with env var expansion) or in a separate secrets store? Env var expansion is
   the v1 answer; revisit if secrets management becomes a concern.
2. **Upstream server lifecycle** — stdio subprocesses started by the proxy exit
   when `mux mcp` exits. For SSE servers that are already running, the proxy
   just disconnects. Should the proxy optionally restart crashed stdio servers?
   (Probably not — that's Cerberus's job.)
3. **Health checks** — should `mux_health` report upstream server status? Yes,
   but only in Phase 1+ once the client pool exists.
4. **go-toolbroker ownership** — the broker currently has no upstream MCP client
   support. When Phase 1 lands, consider adding `RegisterFromMCPClient()` to
   go-toolbroker so Nanite can use the same pattern. Or keep it in agent-mux
   and expose the merged tool list via the HTTP API.

## Related work

- `go-toolbroker` — intent-aware tool selection; `LocalBroker.RegisterTools()`
  is the insertion point for proxied tools
- `go-mcp` (hollis-labs, if/when extracted) — shared MCP client/server helpers
- ADR 0019 — MCP stdio adapter (the foundation this extends)
- Hadron `internal/mcpadapter` — reference implementation of the server side
- `mark3labs/mcp-go v0.47.0` client package — `NewStdioMCPClient`,
  `NewSSEMCPClient`, `ListTools`, `CallTool` — all available today
