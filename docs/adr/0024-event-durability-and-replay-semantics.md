# ADR-0024 — Event Durability and Replay Semantics

**Status:** Accepted
**Date:** 2026-04-23
**Supersedes:** —
**Extends:** ADR-0007 (event bus drop policy), ADR-0021 (MCP proxy observability)

---

## Context

Phase 3 introduces external observation APIs: session event history, proxy/tool
call event queries, attachment history, and checkpoint enumeration. Before
adding those HTTP endpoints and MCP tools, consumers need a single source of
truth for:

1. **What survives a process restart** — durable artifacts queryable after the
   daemon comes back.
2. **What is live-only** — telemetry that exists only while the daemon runs or
   a subscriber is connected.
3. **How to reconnect and recover state** — the cursor/replay mechanisms
   available for each artifact type.
4. **What happens when persistence fails** — failure modes at each storage
   layer.

Without this contract, observation API implementors make inconsistent
assumptions, consumers design reconnect logic for guarantees that do not exist,
and the proxy event store remains ambiguously "transitional."

This ADR also resolves the Phase 3 deferred item from ADR-0021:
> *"SQLite persistence for tool call events"* — the `proxy_events` table
> (migration 0011) is now the contracted durable store. The in-memory
> `ToolCallEventStore` remains a live-subscription cache, not the source of
> truth for queries.

---

## Decision

### 1. Durability Matrix

The following table covers every observable artifact type in the system.
"Durable" means the artifact survives a full daemon process restart.

| Artifact | Durable? | Backing Store | Replay Mechanism | Retention Limit | Drop Behavior |
|---|---|---|---|---|---|
| **Session lifecycle** (state transitions) | Yes | `events` table (`scope=session`, `kind=session.state_changed`) | `GET /events/stream?since_seq=N` or `GET /sessions/{id}/events` (query by session ID) | Unbounded (grows with sessions) | None — every transition persisted |
| **Terminal outcomes** (`exit_code`, `ended_at`) | Yes | `sessions` table columns | `GET /sessions/{id}` — current state snapshot | Unbounded | None — scalar field update |
| **Client attach/detach** | Yes | `client_attachments` table (`attached_at`, `detached_at`) | `GET /sessions/{id}/attachments` | Unbounded | Stale attachments (NULL `detached_at`) swept on daemon start |
| **PTY output stream** | No | In-flight bytes only | None — re-attach gets the live stream from the attach point forward | N/A — not stored | All bytes lost on disconnect or restart |
| **Messages / broker envelopes** | Yes | `messages` table (go-messaging) | `GET /messages/inbox`, `GET /messages/thread/{id}`, `GET /messages/{id}` | Unlimited — no expiry | None — envelope creation is atomic; bus notification follows |
| **Broker envelope created/replied events** | Yes | `events` table (`scope=broker`) | `GET /events/stream?since_seq=N` | Unbounded | None |
| **Tool call / proxy events** | Yes | `proxy_events` table (ring-buffered, max 2000 rows) | `GET /proxy/events?since=N&session=&server=&tool=&errors_only=&limit=` | 2000 rows (oldest evicted on overflow) | Oldest row deleted before insert when at capacity |
| **Checkpoints** (full payload) | Yes | `checkpoints` table | `GET /sessions/{id}/checkpoints` (via `logical_agent_id`) | Unlimited | None |
| **Resume/abort actions** | Yes (derived) | State transition in `sessions` + optional checkpoint in `checkpoints` | Same as session lifecycle + checkpoints | See above | See above |
| **Provider health / ping signals** | No | In-memory only | None | N/A | Lost on disconnect or restart |
| **Daemon lifecycle events** (`daemon.started`, `daemon.shutdown_*`) | Yes | `events` table (`scope=daemon`) | `GET /events/stream?since_seq=N` | Unbounded | None — persisted before bus notify |
| **Bus pub/sub delivery** | No | In-memory subscriber channels | Re-subscribe with `since_seq=<last_seen>` to replay from `events` table | N/A | Drop-oldest per subscriber; chronic-drop eviction (ADR-0007) |

**Key: durable = the artifact is in SQLite and persists across restart. Live-only = exists only in-process.**

---

### 2. Replay Semantics

#### 2.1 `events` table — SSE cursor (`since_seq`)

The `events` table uses a SQLite `AUTOINCREMENT` primary key (`id`). The event
bus assigns `Seq = id` after a successful insert. Every event row is
immutable once written.

**Cursor contract:**

| `since_seq` value | Behavior |
|---|---|
| `0` (or absent) | Full replay: all rows returned, then live events appended |
| `N > 0` | Replay rows with `id > N`, then live events appended |
| Very large `N` (beyond max `id`) | Live-only — no replay rows returned |

**Ordering guarantee:** Events are returned `ORDER BY id ASC`. The `id`
column is the total order within the `events` table. There is no secondary
ordering; wall-clock `at` timestamps are informational only.

**Persistence-before-notification invariant** (from ADR-0007): A row is
inserted and committed before the `Seq` is handed to the bus. A persist
failure does not notify subscribers. This means: if a consumer receives
`seq=N` from the live bus, row `N` is guaranteed to exist in the store.

**Gap handling:** If a subscriber is evicted by the bus drop policy and
reconnects with `since_seq=<last_seen_seq>`, the SSE endpoint replays all
rows with `id > last_seen_seq` before resuming live delivery. Gaps in the
live stream are recovered from durable storage; no events are permanently
lost from the `events` table (unbounded retention).

#### 2.2 `proxy_events` table — cursor by `id`

The `proxy_events` table uses a SQLite `AUTOINCREMENT` primary key. It is
a ring buffer: when the row count reaches 2000, the oldest row is deleted
before each new insert. The autoincrement `id` is monotonically increasing
and never reused.

**Cursor contract for `GET /proxy/events`:**

| Parameter | Default | Semantics |
|---|---|---|
| `since` | `0` | Return rows with `id > since` |
| `session` | (none) | Filter by `session_id` |
| `server` | (none) | Filter by `server` |
| `tool` | (none) | Filter by `tool_name` |
| `errors_only` | `false` | Only rows where `ok=false` |
| `limit` | `100` | Max rows returned |

**Ordering:** Rows returned `ORDER BY id ASC`. Because the ring buffer
evicts the oldest rows, a cursor replayed after a large volume of calls
may miss the oldest events that were evicted. Consumers must accept
at-most 2000-row history per restart cycle, or reduce query frequency.

**In-memory cache (`ToolCallEventStore`) vs. durable table:**  
The in-memory ring buffer in `internal/mcpadapter/events_store.go`
continues to exist as a **live-subscription cache** for the TUI feed and
real-time observers. It is NOT the query source for `mux_events_tool_calls`
or any HTTP API. After this ADR, those query paths use the `proxy_events`
table. See §4 for the specific wiring change.

#### 2.3 Reconnect scenario walkthrough

**Scenario:** A Clockwork orchestrator connects to the SSE event stream,
processes events up to `seq=450`, then the network drops. Meanwhile the
daemon processes 30 more events (`seq=451–480`). The orchestrator
reconnects.

```
Step 1 — Orchestrator reconnects, sends: GET /events/stream?since_seq=450
Step 2 — SSE handler queries: SELECT * FROM events WHERE id > 450 ORDER BY id ASC
Step 3 — Replays rows 451–480 to orchestrator (in order)
Step 4 — SSE handler switches to live delivery: subscribes to bus from current seq
Step 5 — New events (481+) arrive on the live channel and are forwarded
Step 6 — Orchestrator is fully caught up with no gaps
```

No events are lost in this path because the `events` table retains all rows.

---

### 3. Consumer Reconnect Scenario

**Full reconnect after daemon restart:**

```
State before restart:
  - sessions table: session S1 in state "running", pid=12345
  - events table: rows 1–600 (session lifecycle + daemon events)
  - proxy_events table: rows (id 1–2000, ring-buffered)
  - client_attachments: S1 attached at T=10:00, detached_at=NULL (stale)
  - messages table: 12 messages, some consumed
  - checkpoints: 3 checkpoints for logical agent LA-1

Daemon restarts:
  Step 1 — store.Open() runs migrations (no-op if current)
  Step 2 — Daemon startup sweeps stale attachments: sets detached_at=now
            for all client_attachments WHERE detached_at IS NULL
  Step 3 — Session S1 is in unknown state (pid=12345 is gone).
            Runtime reconciles: marks S1 "stopped" (or "failed") and sets ended_at.
            This transition is written to events table as seq=601.
  Step 4 — daemon.started event written: seq=602

Consumer re-attaches:
  Step 5 — Consumer calls GET /sessions to see current session list → sees S1=stopped
  Step 6 — Consumer calls GET /sessions/S1/events → gets all lifecycle events for S1
  Step 7 — Consumer calls GET /proxy/events?session=S1&since=0 → gets last ≤2000 tool calls
  Step 8 — Consumer calls GET /sessions/S1/checkpoints → gets all 3 checkpoint payloads
  Step 9 — Consumer calls GET /sessions/S1/attachments → sees attach + daemon-sweep detach
  Step 10 — Consumer calls GET /messages/inbox?to=<urn> → any undelivered messages
  Step 11 — Consumer calls GET /events/stream?since_seq=600 → receives seq=601,602 replay
             then subscribes to live events going forward

Recovery complete. Consumer has full state.
```

**What cannot be recovered:**
- PTY byte output (not stored; the session is stopped, so moot)
- Provider health signals (in-memory; provider state resets on restart)
- In-memory bus subscriptions (consumer must re-subscribe; replay covers the gap)

---

### 4. Decision: `proxy_events` Table Status

**The `proxy_events` table (migration 0011) is the contracted durable store
for tool call events.** It is not transitional.

**What this means:**

- `mux_events_tool_calls` (MCP tool) MUST query `proxy_events` as its primary
  source. The in-memory `ToolCallEventStore` may be consulted for events too
  recent to have been flushed, but the proxy_events table is authoritative.
- `GET /proxy/events` (new Phase 3 endpoint) queries `proxy_events` directly.
- `LoggingMiddleware` continues to write to both the in-memory store (for TUI
  live feed) and to `proxy_events` via `store.AppendProxyEvent()`. This is the
  current code path; no architectural change needed for the write side.
- The in-memory `ToolCallEventStore` ring buffer is retained as a live cache
  for the TUI `ToolCallFeedScreen`. It is NOT exposed as a query API.

**Retention:** `proxy_events` is bounded to 2000 rows by the trim-after-insert
policy in `store.AppendProxyEvent()`. This is appropriate for the current use
case (recent call history for debugging). If longer retention is needed, it is
a Phase 4 concern (see §8).

---

### 5. Decision: PTY Output — Not Stored

PTY output is not stored. The attach stream is live-only.

**Rationale:**
- Storing PTY output requires byte-stream storage, seek/replay, and potential
  large storage growth (interactive sessions can produce megabytes of output).
- The current `client_attachments` table records connect/disconnect
  metadata. The byte content is not in scope.
- Terminal interaction sessions (human-in-the-loop) do not require replay for
  correctness; the session state is recoverable from the `events` and
  `sessions` tables.

**Gap acknowledged:** External observers (Nanite plugin, dashboard) cannot
replay terminal output after disconnect. This is a known limitation.

**Phase 4 follow-up (if desired):** A `pty_output` table (or external log
file per session) with offset tracking could enable byte-stream replay. This
would require significant implementation and storage budget decisions. It is
explicitly out of scope for Phase 3.

---

### 6. Decision: Retention Limits

| Store | Retention Policy |
|---|---|
| `events` table | Unbounded — rows grow with daemon lifetime. No explicit expiry or size cap. |
| `proxy_events` table | Ring-buffered at 2000 rows. Oldest row evicted on overflow. |
| `checkpoints` table | Unbounded — every checkpoint retained for the lifetime of the logical agent. |
| `messages` table | Unbounded — messages are never deleted, only delivered/consumed/canceled. |
| `sessions` table | Unbounded — every session row retained (lightweight; one row per session). |
| `client_attachments` table | Unbounded — one row per attach event. Stale rows updated on daemon start. |

**Rationale for unbounded `events`:** Session lifecycle events are small JSON
blobs. An active system with 1000 sessions/day generates ~10k events/day at
~500 bytes/event ≈ 5 MB/day. A 90-day retention sweep is a Phase 4 addition
if storage pressure is observed.

**Rationale for 2000-row proxy buffer:** Tool call events can be frequent
(many tool calls per second in proxy-heavy workloads). Unbounded storage
would require the write path to consider storage pressure on every proxy call.
2000 rows cover typical debugging windows; a persistent log is Phase 4.

---

### 7. Failure Modes

#### 7.1 `events` table write failure

**Path:** `store.InsertEvent()` → SQLite write.

**Behavior:** The runtime manager's event publish fails. The bus notification
does NOT fire (persistence-before-notification invariant). The session state
transition still succeeds (the transition and event insert are in separate
operations; the state update is committed to `sessions` before the event
insert is attempted).

**Impact:** A gap in the `events` table. The session transitions to the new
state, but no `session.state_changed` event row exists for that transition.
SSE consumers will not see this transition via replay; they can still read
current state from `GET /sessions/{id}`.

**Recovery:** No automatic recovery. The missing event is unrecoverable.
Operator action: investigate SQLite WAL corruption, disk full, or lock
contention. The session state itself is consistent.

#### 7.2 `proxy_events` write failure

**Path:** `store.AppendProxyEvent()` → SQLite write (called from
`LoggingMiddleware`).

**Behavior:** The tool call completes normally. The proxy event is not
persisted. The in-memory `ToolCallEventStore` still receives the event
(bus publish succeeds if the table write failure happens after or
independently).

**Impact:** A gap in `proxy_events`. The tool call is not queryable via
`GET /proxy/events` or `mux_events_tool_calls` after restart.

**Recovery:** None. The event is permanently absent from the durable store.
Operator action: same as above (disk, lock). TUI may still show the call
from the in-memory store during the current daemon run.

#### 7.3 `messages` table write failure

**Path:** `messaging_store.Send()` → SQLite write (WAL-mode, single writer).

**Behavior:** `Send` returns an error. The envelope is never persisted.
No bus notification. The HTTP/MCP caller receives `internal_error` (HTTP 500).

**Impact:** Message is lost. The sender must retry. No partial delivery risk
(write is atomic).

**Recovery:** Caller retry. The `messages` table is WAL-mode with
`busy_timeout=5000ms` to absorb transient lock contention.

#### 7.4 SQLite database unavailable (disk full, file missing)

**Behavior:** `store.Open()` fails at daemon start. The daemon exits with an
error log. No data is written or read.

**Impact:** Complete service unavailability. All durable stores are
inaccessible.

**Recovery:** Operator restores disk space or database file and restarts the
daemon. SQLite WAL mode prevents partial-write corruption; a clean restart
after disk recovery is safe.

#### 7.5 Bus drop-oldest eviction (consumer evicted)

**Behavior:** A slow SSE subscriber is evicted after exceeding `MaxConsecDrops`
(ADR-0007). Its channel is closed. The HTTP connection drops with a stream
termination.

**Impact:** Consumer loses the live stream. No events are lost from the
durable `events` table.

**Recovery:** Consumer reconnects with `since_seq=<last_seen>`. All events
since the last-seen sequence are replayed from the `events` table.

---

### 8. Follow-up Implementation Tasks (CW-20260423-0022)

The following are the specific packages and functions needed for the Phase 3
observation API implementation task.

#### 8.1 `GET /sessions/{id}/events`

- **Existing:** `store.ListEventsBySession(ctx, sessionID)` — already
  implemented. Returns all `events` rows for a given `session_id`.
- **Needed:** HTTP handler in `internal/api/` wiring this to a route.
- **MCP tool:** `mux_session_events` — wraps the same store query.

#### 8.2 `GET /sessions/{id}/checkpoints`

- **Existing:** `store.ListCheckpointsByLogicalAgent(ctx, logicalAgentID)` —
  requires resolving `session_id → logical_agent_id` first via
  `store.GetLogicalAgentByLastSession` or a direct join.
- **Needed:** HTTP handler; may need a helper
  `store.ListCheckpointsBySession(ctx, sessionID)` that joins
  `sessions → logical_agents → checkpoints`.
- **MCP tool:** `mux_session_checkpoints`.

#### 8.3 `GET /proxy/events` (new endpoint)

- **Existing:** `store.AppendProxyEvent()` writes to `proxy_events`. No query
  path exists yet.
- **Needed:**
  - `store.ListProxyEvents(ctx, filter ProxyEventFilter)` with fields:
    `SessionID`, `Server`, `ToolName`, `SinceID`, `ErrorsOnly`, `Limit`.
  - HTTP handler in `internal/api/` exposing `GET /proxy/events`.
  - Wire to `mux_events_tool_calls` MCP tool as the durable backend
    (replace in-memory `ToolCallEventStore` query with table query).

#### 8.4 `GET /sessions/{id}/attachments`

- **Existing:** `store.ListClientAttachments(ctx, sessionID)` — check if
  implemented; the `client_attachments` table exists (migration in place).
  If missing, add `ListClientAttachments` to `internal/store/sessions.go`.
- **Needed:** HTTP handler; lightweight endpoint.
- **MCP tool:** `mux_session_attachments`.

#### 8.5 Wire `proxy_events` as authoritative backend for `mux_events_tool_calls`

- **File:** `internal/mcpadapter/tool_events.go` — `registerToolCallEventsTool`.
- **Change:** Replace `eventStore.List(filter)` (in-memory) with
  `store.ListProxyEvents(ctx, filter)` (SQLite). The in-memory store is
  retained for TUI only.
- **Signature impact:** The MCP tool already accepts `session_id`, `server`,
  `tool_name`, `errors_only`, `limit` as filter params. Add `since` (maps to
  `SinceID int64`).

#### 8.6 MCP tools summary

| Tool | Backed by | Status |
|---|---|---|
| `mux_events_tool_calls` | `proxy_events` table (after 8.5) | Exists; needs rewire |
| `mux_session_events` | `events` table | New |
| `mux_session_checkpoints` | `checkpoints` table | New |
| `mux_session_attachments` | `client_attachments` table | New |
| `mux_proxy_events` | `proxy_events` table | New (HTTP + MCP) |

---

## Rationale

**Why not store PTY output?**  
Session correctness does not depend on byte-stream replay. State machines
(sessions, agents, messages) are fully recoverable from SQLite. PTY replay
is a UI/developer experience feature that carries significant storage and
implementation cost. Phase 4 can revisit with explicit scope and budget.

**Why ring-buffer proxy_events at 2000 rows?**  
Tool call events can occur at sub-second frequency in active proxy sessions.
A 2000-row buffer covers the recent debugging window (~seconds to minutes)
without unbounded growth. The tradeoff — losing oldest events under load —
is acceptable because proxy event replay is diagnostic, not operational.

**Why is the events table unbounded?**  
Session lifecycle events are infrequent (order of tens per session) and small.
An unbounded table grows predictably and can be swept in a future maintenance
task without breaking any existing API contract. Imposing a retention limit now
would complicate the SSE replay guarantee (consumers could lose events that
are within the `since_seq` window but have been swept).

**Why keep the in-memory ToolCallEventStore?**  
The TUI feed uses a polling + ring-buffer model that works well with the
in-memory store. Replacing the TUI feed with a SQLite-backed poller would add
query latency and complexity with no benefit to the TUI's use case (live feed
of the last N events). The in-memory store is retained as an implementation
detail of the TUI, not exposed as a contract surface.

---

## Consequences

### Positive

- External consumers have a single authoritative answer for what survives
  restart: everything in SQLite (yes), everything in-memory (no).
- The SSE `since_seq` cursor provides lossless replay for all session and
  daemon lifecycle events.
- `proxy_events` is now the contracted source for tool call queries, removing
  the "transitional" ambiguity from ADR-0021.
- Implementation task (CW-20260423-0022) has specific package/function targets
  and no open design questions.

### Negative / Trade-offs

- PTY output replay is explicitly not provided. Dashboards and replay tools
  cannot reconstruct terminal sessions.
- `proxy_events` ring buffer means very high-volume tool call sessions (>2000
  calls before a query) will lose the oldest events. Consumers should query
  frequently or accept bounded history.
- `events` table growth is unbounded. Retention sweep is deferred to Phase 4;
  operators with long-running daemons may see megabytes of event rows.

### Open Questions / Deferred

- **PTY output storage** — explicit Phase 4 candidate. Requires byte-stream
  table design, per-session offset tracking, and retention policy.
- **events table retention sweep** — Phase 4. Simple `DELETE FROM events WHERE
  at < datetime('now', '-90 days')` on daemon start is the expected shape.
- **proxy_events capacity tuning** — 2000 is a reasonable default. If Phase 3
  usage reveals it is too small (e.g., Clockwork runs 50 tool calls/second),
  expose as a config option rather than hard-coding.
- **Proxy event verbose logging** — ADR-0021 deferred opt-in full arg logging.
  Still deferred. The `proxy_events` table stores `args_schema_fp` only.

---

## References

- ADR-0007 — Event Bus Drop Policy (drop-oldest, persistence-before-notification)
- ADR-0015 — Checkpoint Payload Schema
- ADR-0018 — Broker Envelope Types and Correlation-ID Scheme
- ADR-0021 — MCP Proxy Observability (Phase 2; in-memory ring buffer)
- ADR-0023 — Message Routing Contract
- `internal/store/events.go` — `InsertEvent`, `ListEventsBySession`
- `internal/store/proxy_events.go` — `AppendProxyEvent`
- `internal/mcpadapter/events_store.go` — in-memory `ToolCallEventStore`
- `internal/mcpadapter/tool_events.go` — `mux_events_tool_calls` tool
- `docs/db/migrations/0011_proxy_events.sql` — `proxy_events` table schema
- CW-20260423-0022 — Phase 3 observation API implementation task
- CW-20260423-0027 — this decision task
