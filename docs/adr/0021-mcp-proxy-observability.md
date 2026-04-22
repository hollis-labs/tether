# ADR 0021 — MCP Proxy Observability (Phase 2)

**Date:** 2026-04-22  
**Status:** Accepted  
**Relates to:** ADR 0020 (MCP Proxy / Tool Aggregator — Phase 1)  
**Sprint:** SP-20260421-0003

---

## Context

Phase 1 (ADR 0020) delivered tool aggregation: `mux mcp --proxy` proxies upstream MCP servers and presents their tools as its own. Every proxied tool call flows through `ProxyRouter.Handle()`, giving Phase 2 a natural interception point.

Phase 2 goal: every proxied call emits a structured event through the existing `internal/events` Bus so the call is observable — in the TUI, via a query tool, and eventually in persistent storage.

Four design decisions were required:

1. **How to instrument the proxy** — inline vs. middleware interface
2. **Where to buffer events** — SQLite vs. in-memory
3. **How much of the call to log** — full args vs. schema fingerprint
4. **How to scope the TUI feed** — per-session vs. all-sessions

---

## Decision 1 — ToolCallMiddleware interface (not inline instrumentation)

**Context:** The simplest approach is to add `slog` calls and `bus.Publish` calls directly inside `ProxyRouter.Handle()`. This has zero abstraction overhead.

**Decision:** Introduce a `ToolCallMiddleware` interface in `internal/mcpadapter/middleware.go` with a `buildMiddlewareChain` helper, and implement `LoggingMiddleware` as the first concrete middleware. The chain is composed in `ProxyRouter.Handle()` via `NewProxyRouterWithMiddleware`.

```go
type ToolCallMiddleware interface {
    Handle(ctx context.Context, call mcp.CallToolRequest, next ToolCallHandler) (*mcp.CallToolResult, error)
}
```

**Rationale:**
- **Composability.** Phase 3 adds `PIIRedactionMiddleware` and `RateLimitMiddleware` without touching `ProxyRouter`. Each concern stays in its own file.
- **Testability.** Middleware can be unit-tested independently with a mock `ToolCallHandler`.
- **Extractability.** The interface can be lifted to a future `go-mcp-middleware` shared package (ADR 0020 Open Question §4) without API changes.
- **Precedent.** Identical to `go-toolbroker`'s `Enricher` pattern — same team, consistent idiom.

The interface is kept on `mcp.CallToolRequest` (not a custom `ToolCall` struct) to avoid unnecessary abstraction in Phase 2. If Phase 3 needs request mutation, the interface can be extended.

---

## Decision 2 — In-memory ring buffer for Phase 2 (not SQLite)

**Context:** The `internal/events` Bus already persists to SQLite via the `events` table (from Sprint v002-03). A natural Phase 2 option is to query that table for recent tool call events.

**Decision:** `ToolCallEventStore` is an in-memory ring buffer (default capacity: 1000 events). It subscribes to the Bus for `EventTypeToolCallEnd` events and appends them locally. The `mux_events_tool_calls` tool queries this store.

**Rationale:**
- **Zero migration cost.** The existing `events` table uses a `scope/sessionID/kind/payloadJSON` schema designed for session lifecycle events. Adding tool call events there is unambiguous but requires a migration for any index or column additions.
- **Sufficient for Phase 2 use cases.** TUI feeds and `mux_events_tool_calls` only need recent history (last 1000 events). SQLite query latency adds no value here.
- **Transparent upgrade path.** Phase 3 adds a dedicated `tool_call_events` table and queries it instead. The `ToolCallEventStore` interface stays stable; only the backing implementation changes.
- **Memory cost is bounded.** 1000 `ToolCallEvent` structs ≈ 200–400 KiB depending on error message lengths. Acceptable for a daemon process.

**Deferred to Phase 3:** SQLite persistence, `mux events list` CLI subcommand, cross-restart query.

---

## Decision 3 — args_schema_fingerprint (not raw arg values)

**Context:** Tool call arguments may contain secrets: authentication tokens, API keys, passwords, user data. The LLM passes these as arguments to tools like `vanta_memory_write` or `cerberus_cerberus_start`. Logging full args by default would be a privacy and security hazard.

**Decision:** `LoggingMiddleware` logs only an `args_schema_fingerprint` — an 8-character hex SHA-256 of the sorted arg key names. Arg values are never read or logged.

```
sorted keys: ["bearer_token", "target", "ttl"]
hash input:  "bearer_token,target,ttl"
fingerprint: "a3f2c9d1"
```

**Rationale:**
- **Privacy-safe by default.** Secrets in arg values are never exposed in the event stream, log files, or `mux_events_tool_calls` output.
- **Schema patterns remain visible.** The fingerprint identifies the arg shape — useful for debugging call patterns ("what args did `hadron_run_enqueue` receive?") without exposing values.
- **Stable.** The same fingerprint is produced regardless of arg value changes, so it can be used to correlate calls across time.
- **Opt-in verbose logging is Phase 3.** When a user explicitly enables debug mode, full args can be logged (with value redaction for known secret keys). This is not automatic.

**Implementation:** `ArgsSchemaFP(args json.RawMessage) string` in `internal/mcpadapter/middleware.go`. Called by `LoggingMiddleware.Handle()` before forwarding to `next`.

---

## Decision 4 — TUI feed shows all sessions (not per-session scoped)

**Context:** Phase 1's TUI is session-unaware in the proxy feed — there is no session context when `mux mcp --proxy` runs (sessions are independent from MCP invocations in the current architecture). A per-session feed would require a session identity injected into every tool call context.

**Decision:** `ToolCallFeedScreen` in `internal/tui/detail/tool_call_feed.go` subscribes to the `ToolCallEventStore` (polling every 100ms) and renders all sessions in a single chronological feed.

**Rationale:**
- **Simpler implementation.** No session context threading required through the proxy call path.
- **Sufficient for Phase 2.** Operators monitoring a running proxy want to see all activity — not scoped to a session they would need to know about in advance.
- **Consistent with existing event panel design.** The existing `internal/events` Bus is also scope-agnostic in Phase 2.

**Deferred to Phase 3:** Per-session scoping when session identity is reliably available in the proxy call context (requires session middleware or context propagation work).

---

## Implementation summary

| File | Purpose |
|------|---------|
| `internal/events/tool_call.go` | `ToolCallEvent` struct, `EventTypeToolCallStart/End` constants |
| `internal/mcpadapter/middleware.go` | `buildMiddlewareChain`, `ArgsSchemaFP` |
| `internal/mcpadapter/middleware_logging.go` | `LoggingMiddleware`, context key helpers |
| `internal/mcpadapter/proxy.go` | `NewProxyRouterWithMiddleware`, chain wiring in `Handle()` |
| `internal/mcpadapter/proxy_adapter.go` | `ProxyOptions`, `RunWithProxyOpts`, event store subscription |
| `internal/mcpadapter/events_store.go` | `ToolCallEventStore` ring buffer + `Subscribe` |
| `internal/mcpadapter/tool_events.go` | `registerToolCallEventsTool` — `mux_events_tool_calls` |
| `internal/tui/detail/tool_call_feed.go` | `ToolCallFeedScreen` — live BubbleTea panel |
| `cmd/mux/mcp.go` | Wire `ProxyOptions{Bus, EventStore}` when `--proxy` active |

---

## Consequences

### Positive

- Every proxied tool call is observable with structured metadata within ~1ms of completion.
- `mux_events_tool_calls` enables MCP clients to introspect recent proxy activity as a native tool call.
- TUI feed provides real-time visibility without a separate monitoring tool.
- Privacy-safe by default: no arg values ever appear in the event stream.
- Middleware chain is extensible: PIIRedaction and RateLimiting are Phase 3 drop-ins.

### Negative / Trade-offs

- **Session ID is best-effort.** Without a session context injected into the MCP call path, `session_id` in `ToolCallEvent` is empty for most calls. Phase 3 fixes this with session middleware.
- **100ms TUI poll lag.** The feed screen polls every 100ms rather than receiving push notifications. Acceptable for Phase 2; Phase 3 can use a BubbleTea channel bridge to the Bus for zero-lag updates.
- **Ring buffer is not durable.** A restart loses all buffered events. Phase 3 adds SQLite persistence.

### Deferred to Phase 3

- SQLite-backed `ToolCallEventStore` with cross-restart query.
- `mux events list` CLI subcommand.
- Per-session TUI feed with session identity propagation.
- `PIIRedactionMiddleware` — strip known secret field names from fingerprint output.
- `RateLimitMiddleware` — per-upstream and per-tool rate limits.
- Opt-in verbose logging with arg value capture (explicit operator enablement required).

---

## References

- [ADR 0020 — MCP Proxy / Tool Aggregator](./0020-mcp-proxy-aggregator.md)
- `internal/mcpadapter/middleware.go` — interface definition
- `internal/mcpadapter/middleware_logging.go` — LoggingMiddleware implementation
- `internal/mcpadapter/events_store.go` — ring buffer
- `internal/tui/detail/tool_call_feed.go` — TUI panel
