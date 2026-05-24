# ADR 0020 — MCP Proxy / Tool Aggregator

**Date:** 2026-04-22  
**Status:** Accepted  
**Relates to:** ADR 0019 (MCP stdio adapter)  
**Extended by:** ADR 0021 (MCP Proxy Observability — Phase 2 middleware, ring buffer, args fingerprint, TUI feed)

---

## Context

An agent session talks to multiple MCP servers — `agent-mux`, `hadron`, `vanta`, `cerberus`, and others. Today each server requires its own entry in the client's `.mcp.json`. As the portfolio grows this creates several problems:

1. **Tool namespace pollution.** The LLM sees 80+ tools in a flat bag. Agents have no way to scope a query to "only hadron tools".
2. **No observability.** Tool calls route directly upstream with no log, no policy, no audit trail.
3. **Client configuration drift.** Every machine that runs an agent needs `.mcp.json` kept in sync with the current set of servers.
4. **agent-mux is just another entry.** There is no single trusted gateway the agent can anchor on.

The vision: **one process the agent talks to**. `mux mcp` becomes the single MCP entry in `.mcp.json`; all other servers are reached through it.

---

## Decision

### Phase 1 scope — aggregation only

Phase 1 delivers aggregation: mux proxies upstream servers and presents their tools as its own. No middleware, no broker integration, no observability events (those land in Phase 2).

### Decisions locked

| Decision | Choice | Rationale |
|---|---|---|
| Activation | `--proxy` flag (opt-in) | Proxy startup spawns subprocesses and waits for `ListTools` RTT. Always-on would slow down every `mux mcp` invocation even when upstreams are unused. |
| Catalog location | `~/.tether/catalog/mcp-servers/*.yaml` | Consistent with existing `providers/`, `agents/`, `launches/` sibling directories. One file per server, owned by the user. |
| Transport support | `stdio` (primary) + `sse` (secondary) | stdio works everywhere a subprocess can be launched. SSE supports remote/multi-client scenarios (Vanta, hosted services). |
| Tool name policy | Unchanged upstream names | Portfolio prefix convention (`hadron_*`, `vanta_*`, `mux_*`) is already in place and prevents collisions in practice. |
| Collision handling | Warn + `<serverID>__<toolName>` disambiguation | Keeps both tools accessible. Warns at startup so operators can fix the root cause. |
| Native tools | Always registered in proxy mode | `mux_*` tools are available regardless of upstream state. Upstream failure does not break mux self-management. |

### Implementation

```
internal/config/mcp_server.go          MCPServerEntry type + LoadMCPServers loader
internal/mcpadapter/registry.go        ToolRegistry — thread-safe merged tool set
internal/mcpadapter/client_pool.go     ClientPool — spawn upstreams + ListTools
internal/mcpadapter/proxy.go           ProxyRouter — CallTool forwarding
internal/mcpadapter/proxy_adapter.go   Adapter.RunWithProxy — wires it all together
cmd/mux/mcp.go                         --proxy flag
```

The `ToolCallMiddleware` interface is defined in `proxy.go` but has no implementations in Phase 1. It is the extension point for Phase 2.

### Catalog entry format

```yaml
# ~/.tether/catalog/mcp-servers/hadron.yaml
id: hadron
transport: stdio
command: /usr/local/bin/hadrond
args: [mcp]
env:
  HADRON_TOKEN: "${HADRON_TOKEN}"   # ${VAR} expanded at load time
scopes: [run.write, schedule.write]
tags: [automation, ci, blueprints]
enabled: true                        # default: true

# ~/.tether/catalog/mcp-servers/vanta.yaml
id: vanta
transport: sse
url: "http://localhost:8090/mcp/sse"
token: "${VANTA_TOKEN}"
tags: [memory, knowledge, context]
```

### Usage

```bash
mux mcp --proxy
```

```json
{
  "mcpServers": {
    "tether": {
      "command": "mux",
      "args": ["mcp", "--proxy"],
      "env": {
        "AGENT_MUX_MCP_TOKEN": "...",
        "HADRON_TOKEN": "...",
        "VANTA_TOKEN": "..."
      }
    }
  }
}
```

---

## Consequences

### Positive

- Single `.mcp.json` entry; adding a new upstream requires only a new `mcp-servers/*.yaml` file.
- Foundation for Phase 2 observability (every tool call flows through `ProxyRouter`).
- Foundation for Phase 3 middleware (PIIRedactionMiddleware, RateLimitMiddleware).
- `mux_catalog_list_mcp_servers` gives the LLM visibility into what is aggregated and its connection state.

### Negative / Trade-offs

- **Startup latency** when `--proxy` is active: subprocess spawn + MCP handshake + `ListTools` RTT per upstream (mitigated by concurrent startup in `ClientPool.Start`).
- **Single point of failure**: if `mux mcp` process crashes all tool access is lost (same risk as any gateway). Mitigated by Cerberus supervision.
- Upstream tool lists are **snapshot at startup** — dynamic tool list changes in the upstream are not reflected until restart.

### Deferred to Phase 2

- `ToolCallMiddleware` implementations: logging, tracing, rate limiting.
- SSE events for tool call lifecycle (request, response, error).
- Upstream health-check polling and auto-reconnect.

### Deferred to Phase 3

- `PIIRedactionMiddleware` — strip sensitive fields from tool results.
- `RateLimitMiddleware` — per-upstream and per-tool rate limits.
- `go-mcp-middleware` library extraction (reusable outside agent-mux).

---

## Open Questions

1. **Token/scope propagation.** Today `${VAR}` expansion at load time is the answer. Long-term: should per-tool scopes be enforced at the proxy layer (reject calls to tools not in `entry.Scopes`)? Deferred to Phase 2 policy design.

2. **Upstream server lifecycle.** When a stdio subprocess crashes, the proxy returns `IsError: true` on subsequent calls. Restart is Cerberus's responsibility. Should the proxy attempt reconnect? Decision: no auto-reconnect in Phase 1 — keep the proxy stateless with respect to process management.

3. **`mux_health` upstream aggregation.** Should `mux_health` report upstream server health in proxy mode? Phase 1+: add `upstream_servers` field to `mux_health` response when `--proxy` is active.

4. **`go-toolbroker` integration.** Future: `RegisterFromMCPClient(client, serverID)` in go-toolbroker so the broker can route calls without going through agent-mux. Deferred — needs broker design work first.

---

## References

- [ADR 0019 — MCP stdio adapter](./0019-mcp-stdio-adapter.md)
- [`mark3labs/mcp-go v0.47.0`](https://github.com/mark3labs/mcp-go)
- `docs/mcp-proxy-spec.md` (spec that drove this implementation)
