# ADR 0035 — `mux mcp` Routes Session-Mutating Tools Through the Daemon

**Status:** Accepted
**Date:** 2026-05-12
**Supersedes:** —
**Superseded by:** —
**Related:** ADR 0002 (daemon transport), ADR 0030 (long-lived integration), ADR 0034 (ACP surface)

## Context

Pre-v005-09, `mux mcp` was built as a sibling app instance to the daemon: the subcommand spun up its own `app.New()` against the same catalog filesystem and session store, talking to its own in-process `Service` rather than routing through the running `muxd`. For read-only catalog queries this was fine. For session-mutating operations (`mux_session_create / launch / stop / send_input / send_turn / resize`, `mux_logical_agent_resume`) it produced a **split-brain**: two processes owned the same session state, but only one (the daemon) held the live `RuntimeManager` with the actual subprocess handles.

The most visible symptom was the v005-05 `SweepStaleSessions` self-clobber: `app.New()` ran sweep at startup, marked any sessions in `running` state with no live PID as `failed`. When `mux mcp` was spawned (e.g., by an MCP client) while the daemon held a real running session, mux mcp's `app.New()` saw the session in `running` state but couldn't see the daemon's process — and clobbered it as failed.

v005-05 fixed the symptom by moving sweep out of `app.New()` into a daemon-only `ReconcileStaleState()` hook. The underlying split-brain remained. Captured as `followup_mux_mcp_proxy_session_mutating_tools_to_daemon`.

The v005-09 ACP work needed a daemon-client abstraction anyway (ACP MVP requires a running daemon — no in-process fallback for session lifecycle), so closing this gap fell naturally inside the same sprint.

## Decision

### 1. Session-mutating MCP tools route through `internal/client.Client` over UDS

The `mcpadapter.Adapter` gains an optional `client *client.Client` field. When non-nil, the eight session-mutating handlers dispatch via the daemon-client instead of the in-process service:

| MCP tool | Daemon-client method |
|---|---|
| `mux_session_create` | `CreateSession` / `CreateSessionWithInput` (depending on Tier-2 fields) |
| `mux_session_launch` | `LaunchSession` |
| `mux_session_stop` | `StopSession` |
| `mux_session_wait` | `WaitSession` |
| `mux_session_send_input` | `SendInput` |
| `mux_session_send_turn` | `SendTurn` |
| `mux_session_resize` | `ResizeSession` |
| `mux_logical_agent_resume` | `ResumeLogicalAgent` |

A new constructor `mcpadapter.NewWithDaemon(svc, dc, token, scopes)` wires the client. `mcpadapter.New(svc, token, scopes)` (in-process-only) is preserved for tests.

### 2. Read-only and message handlers stay in-process

| Category | Handlers | Why in-process is fine |
|---|---|---|
| Catalog reads | `mux_catalog_list_*`, `mux_catalog_*` | Filesystem reads; no process state |
| Read-only session inspection | `mux_session_list`, `mux_session_get` | DB reads; no live runtime needed |
| Messages | All `mux_message_*` | DB-backed (SQLite WAL); no process state coupling |
| Health (with caveat) | `mux_session_health` | Reads daemon-only `RuntimeManager` — broken under daemon routing; see §4 |
| Boot generation | `mux_boot_*` | Pure file/template work |

### 3. Daemon-unreachable returns `daemon_unavailable`

When the daemon isn't running, mutating handlers return an ADR 0010 typed error envelope with code `daemon_unavailable` and an actionable message (`"start it with mux daemon up"`). Read-only and message handlers continue to work in-process — a dev-mode `mux mcp` against a stopped daemon can still inspect catalog state.

### 4. `mux_session_health` parity is intentionally incomplete

`handleSessionHealth` reads `app.Service.RuntimeHealth(id)`, which is daemon-only in-memory state. Under daemon-routing the in-process service has no live `RuntimeManager`, so the handler returns `conflict: "session is not currently running"` even when the session IS running on the daemon.

**Why not fix this sprint:** Requires a new daemon API endpoint (`GET /sessions/{id}/health`) + corresponding `internal/client` method + handler refactor. ~30-60 minutes of additional work that doesn't block the v005-09 ACP sprint goal. Captured as `followup_mux_mcp_session_health_daemon_routing` for v005-10 hardening.

### 5. `Client.ResumeLogicalAgent` widened to return full `LaunchResponse`

The previous signature `(string, error)` returned only the new session ID. The MCP handler emits workspace + log + provider metadata, requiring a follow-up `GetSession` round-trip. v005-09 widens to `(api.LaunchResponse, error)` — only consumer was the MCP handler. The daemon endpoint already returns the full envelope; this is a client-side decode change.

## Consequences

### Positive

- Closes `followup_mux_mcp_proxy_session_mutating_tools_to_daemon`.
- `mux mcp` and `mux acp` share the same daemon-routing pattern via `internal/client`, building toward a future `go-protocol-server` lib (captured separately).
- No more split-brain on session state. The daemon is the single owner.
- Read-only fast path preserved — catalog reads don't need to round-trip UDS.

### Negative / deferred

- `mux_session_health` is broken under daemon routing (returns `conflict` even for running sessions). Captured for v005-10.
- Daemon-routed branches in 8 handlers have zero unit-test coverage. The in-process branch retains full coverage; `internal/client.Client` itself is well-tested. Captured as `followup_mux_mcp_daemon_routed_branch_unit_tests` to add `httptest`-based smoke before final beta ship.
- Message handlers + `mux_logical_agent_list` stay in-process — no split-brain there (DB-backed) but a future "everything-through-daemon" pass would unify the surface.

### Operational

- No new flags; `mux mcp` automatically uses daemon routing when the catalog config resolves a daemon listen address (the existing `resolveDaemonAddr()` helper).
- When the daemon is stopped, mutating tools fail cleanly with `daemon_unavailable` (was: silently produced incorrect state).
- Behavior change for callers that ran `mux mcp` without a running daemon for mutating ops — they now get an actionable error instead of split-brain. Considered correct per ADR 0002 (daemon as single owner of session lifecycle).

## References

- Surfaced from v005-05 implementer report.
- Companion: ADR 0034 (ACP surface adoption — same daemon-client abstraction).
- Vanta: `followup_mux_mcp_proxy_session_mutating_tools_to_daemon` (closed by this ADR), `followup_mux_mcp_session_health_daemon_routing`, `followup_mux_mcp_daemon_routed_branch_unit_tests`.
- Tracking root: `agent-workspaces/execution/agent-mux/v005-09-acp-surface/2026-05-12/`.
