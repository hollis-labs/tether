# ADR 0026 — Selective Flat Proxy and Config-Chain MCP Server Filter

**Status:** Accepted — 2026-04-24
**Context:** v0.0.5 — PR #5 (selective flat proxy) + config-chain follow-up
**Deciders:** agent-mux v0.0.5 execution session
**Extends:** ADR 0020 (MCP proxy aggregator), ADR 0021 (MCP proxy observability)

---

## Context

ADR 0020 established `mux mcp --proxy` as a single gateway that aggregates upstream
MCP servers. The initial implementation surfaced _all_ upstream tools as native tools
— a "firehose" — with broker mode (`--broker`) as the opt-in way to reduce context
size by hiding tools behind `mux_discover` + `mux_call`.

Two problems emerged in practice:

1. **Broker mode inverted the ergonomics.** Flat tool calls are the natural LLM interface;
   broker mode required a discover-then-call pattern that added latency and prompt complexity.
   Most agents only need a subset of upstream servers (e.g. `hadron` + `vanta`), not all.

2. **No per-session server scoping.** Agents launched via `mux_session_launch` always
   inherited the server list from the operator's `~/.claude.json` entry. Per-project or
   per-launch overrides required manually editing that file.

---

## Decision

### Phase 1 — Selective flat proxy (`--servers` filter, PR #5)

Deprecate broker mode as the default. Introduce `--servers <id,...>` on `mux mcp`:

- With `--proxy --servers hadron,vanta`: only those servers surface as native tools.
  Undeclared servers remain reachable via `mux_discover` + `mux_call`.
- With `--proxy` (no `--servers`): all servers surface (firehose, unchanged behavior).
- `--broker` still accepted for backward compatibility but is deprecated.
- `MUX_MCP_SERVERS` env var provides the same filter without CLI flag changes — the flag
  wins when both are present.

### Phase 2 — Config-chain injection (this ADR, implemented alongside Phase 1)

Add `mcp.servers` to the project and launch catalog schemas. When `launch.Resolve()`
builds a plan, it resolves the server list with this precedence:

```
project.mcp.servers  >  launch.mcp.servers  >  (absent → no injection)
```

If a list is resolved, it is injected as `MUX_MCP_SERVERS=<comma-joined>` into the
plan's env overrides. The spawned agent process inherits it, and `mux mcp --proxy`
picks it up on startup — no per-operator `~/.claude.json` editing required.

**Schema additions:**

```yaml
# catalog/projects/<id>.yaml
mcp:
  servers: [hadron, vanta]   # overrides any launch-level setting

# catalog/launches/<id>.yaml
mcp:
  servers: [hadron]          # used when project.mcp.servers is absent
```

**Precedence rationale:**
- Project wins over launch because the project's trust boundary is broader — if a
  project owner restricts MCP to specific servers, a launch config must not widen it.
- Launch fallback allows per-task scoping when the project has no opinion.
- Absent at both levels → no `MUX_MCP_SERVERS` set → agent inherits whatever the
  operator's shell environment provides (firehose or none).

### Decisions locked

| Decision | Choice | Rationale |
|---|---|---|
| Deprecate broker as default | Yes | Flat native tools are the natural LLM interface; scoped filtering via `--servers` replaces the broker use case |
| Env var name | `MUX_MCP_SERVERS` | Already established by PR #5; consistent with flag name |
| Injection point | `launch.Resolve()` overrides map | Keeps env composition logic co-located; adapter applies it at start time along with all other overrides |
| Project wins over launch | Yes | Project is the broader trust boundary; launch must not widen project restrictions |
| Empty list = no injection | Yes | Allows the operator's ambient env to take effect (firehose or absent) without forcing catalog authors to always specify |

---

## Implementation

**Files changed:**

- `internal/config/model.go` — `MCPConfig` struct; `MCP MCPConfig` field on `Project` and `Launch`
- `internal/launch/resolver.go` — resolve servers list, inject `MUX_MCP_SERVERS` into overrides
- `internal/launch/resolver_test.go` — `TestResolve_MCPServerChain` (4 sub-tests)
- `cmd/mux/mcp.go` — `--servers` flag, `MUX_MCP_SERVERS` env fallback, `--broker` deprecated (PR #5)
- `internal/mcpadapter/proxy_adapter.go` — `ServerFilter []string` in `ProxyOptions` (PR #5)

---

## Consequences

### Positive

- Operators no longer need to edit `~/.claude.json` to scope MCP servers per project.
- Project catalog is the authoritative source for which servers an agent may use — auditable in git.
- Backward compatible: existing configs without `mcp.servers` are unaffected.
- `--broker` still works for consumers that depend on it; they have a migration path.

### Negative / Trade-offs

- `mcp.servers` is resolved at `Resolve()` time and baked into the plan. Dynamic changes
  to the catalog require a session restart — sessions do not hot-reload their server filter.
- The env var approach couples the launch system to an MCP implementation detail
  (`MUX_MCP_SERVERS`). If the env var name ever changes, both `cmd/mux/mcp.go` and the
  resolver must update in sync.

### Deferred

- **Progressive discovery tuning** (`internal/mcpadapter/discovery.go`): keyword-match
  scoring for `mux_discover` results is raw word-count. Tag-boost weighting and score
  normalization are independent of the config chain and scheduled separately.
- **Runtime server filter updates**: allowing a running session to update its server filter
  without restart. Deferred — requires a signal path from the proxy adapter to the client pool.

---

## References

- [ADR 0020 — MCP Proxy Aggregator](./0020-mcp-proxy-aggregator.md)
- [ADR 0021 — MCP Proxy Observability](./0021-mcp-proxy-observability.md)
- PR #5: Selective flat proxy — `--servers` filter, deprecate `--broker`
