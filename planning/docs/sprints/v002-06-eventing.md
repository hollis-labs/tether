# Sprint v002-06 — Eventing

Epic: [v0.0.2](../epics/v0.0.2-runtime-foundation.md)

**Epic:** [v0.0.2](../epics/v0.0.2-runtime-foundation.md)
**Goal:** Introduce an internal pub/sub event bus (`internal/events`). Publish session lifecycle transitions and broker envelope events. Persist events to the `events` table for replay. Expose the bus through the API (Sprint v002-05's `GET /events/stream`). Make events a first-class primitive rather than an afterthought, per context-pack §05.
**Exit criteria:**
- [x] `internal/events` package exposes a `Bus` with `Publish(ctx, Event)` and `Subscribe(ctx, filter) (<-chan Event, cancel)`.
- [x] Session lifecycle transitions (`created → launching → running → completed/failed/killed`) emit events.
- [x] Broker envelope writes emit events.
- [x] Events persist to `events` table with a stable `stream_seq`. _(id INTEGER PRIMARY KEY AUTOINCREMENT serves as stream_seq — see readiness note.)_
- [ ] `GET /events/stream` (Sprint v002-05) is backed by the bus. _(Deferred to Sprint v002-05. Bus is ready; API endpoint is that sprint's scope.)_
- [x] A subscriber that processes events slowly does not block publishers (bounded buffer with drop-oldest or slow-subscriber eviction, documented).

## Context

Context-pack §05 lists "event bus abstractions" as an explicit reuse-check target and says "event streams are a primitive, not a later bolt-on." Context-pack §08 expects `GET /events/stream` to be subscribable. Context-pack §02 says events are one of Agent Mux's responsibilities ("event source").

Current shape to evolve:
- `internal/store/events` table exists (v0.0.1) but is only written as a debug log, not consumed as a stream.
- Sprint v002-03 (T-v002-s03-05) evolves the table: `scope`, `payload_json`, `stream_seq`.
- Sprint v002-02 introduces per-session attach fan-out. This is deliberately narrow — general event bus is this sprint's job.

When done, every runtime state change is observable from any client subscribing to the bus.

## Tasks

### T-v002-s06-01: Build the event bus in `internal/events`

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, events, bus, foundation]

#### Problem

No shared pub/sub exists. Attach has its own fan-out (T-v002-s02-01); broker writes have no notifier; lifecycle transitions write to the `events` table but nobody streams them.

#### Fix direction

- `internal/events/bus.go`:
  - `type Event struct { Seq int64; At time.Time; Scope string; SessionID string; LogicalAgentID string; Kind string; Payload json.RawMessage }`.
  - `type Bus interface { Publish(ctx, Event) error; Subscribe(ctx, Filter) (<-chan Event, func(), error) }`.
  - `Filter` includes scope set + session/agent ID filters + `SinceSeq int64`.
- In-memory implementation with:
  - Per-subscriber bounded channel (default 1024).
  - Slow-subscriber policy: drop-oldest and record a drop counter; after N consecutive drops, close the channel. Document this clearly.
  - Publisher fan-out is non-blocking; Publish returns even if a subscriber is slow.
- On Publish, the bus also writes to `store.events` (blocking in v0.0.2; async queue is a future optimization).
- On Subscribe with `SinceSeq`, the bus first replays from `store.events` where `stream_seq > SinceSeq`, then switches to live.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/events/` (new package)
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/bus.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/filter.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/persist.go` — the `store.events` sink

#### Acceptance criteria

- [x] Bus `Publish`/`Subscribe` work with scope + session filters.
- [x] Two subscribers see the same events.
- [x] Slow subscriber does not block publisher; drop counter increments.
- [x] Events persist to `store.events` with monotonic `stream_seq`.
- [x] Subscribe with `SinceSeq` replays historical events before switching to live, with no gaps (tested).

#### Test plan

- Unit: publish 1000 events, two subscribers (one fast, one slow), assert fast receives all, slow receives subset with drops logged.
- Unit: `SinceSeq` replay — prepopulate store, subscribe, assert replay then live.
- Race: `go test -race` on concurrent publish + subscribe + cancel.

#### Scope fences

- Do not durably queue per-subscriber — subscribers that disconnect lose state and must resume with `SinceSeq`.
- Do not make the persist sink async in this task. Sync write to `store.events` is simpler and still fast; revisit later if profiling warrants.
- Do not reach for Redis/NATS/etc. This is local, single-process.

#### Relationship

Depends on: T-v002-s03-05 (evolved events table).
Blocks: T-v002-s06-02, T-v002-s06-03, T-v002-s05-05 (events API).

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) ("event bus abstractions … event streams are a primitive"), [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) `/events/stream`.

---

### T-v002-s06-02: Emit session lifecycle events

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, events, lifecycle]

#### Problem

State transitions in `runtime.Manager` (v002-s01-01) update SQLite but don't publish events. Clients can't react to "session just started" or "session failed."

#### Fix direction

- Inject the event `Bus` into `runtime.Manager`.
- On each state transition (`created → launching`, `launching → running`, `running → completed|failed|killed`), publish an event with:
  - `scope: "session"`
  - `session_id`, `logical_agent_id`
  - `kind: "session.state_changed"` (or split into `session.started`, `session.stopped`, etc. — pick one style)
  - `payload: {from, to, exit_code?, reason?}`
- Also emit `session.output_chunk` periodically? **No** — PTY byte firehose is handled by the attach broker, not the event bus. The event bus is metadata, not payload.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/kinds.go` — typed constants for event kinds

#### Acceptance criteria

- [x] Launching a session publishes `session.state_changed` events at each transition.
- [x] A subscriber with `scope=session, session_id=X` receives only events for session X.
- [x] Payload JSON matches documented schema.

#### Test plan

- Unit: stubbed bus, launch + stop via manager, assert event sequence.
- Integration: daemon + API, attach to `/events/stream`, launch, assert events arrive.

#### Scope fences

- Do not publish byte-level output events — attach stream covers that.
- Do not emit events before the store row is written (ordering guarantee: persistence first, then publish).
- Do not invent a new event taxonomy here — document the four or five lifecycle kinds and stop.

#### Relationship

Depends on: T-v002-s01-01, T-v002-s06-01.

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md), [11-task-list-v0-0-2.md](../agent-mux-vfuture-context-pack/11-task-list-v0-0-2.md) milestone 6.

---

### T-v002-s06-03: Emit broker envelope events

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, events, broker]

#### Problem

When a client posts an envelope via `POST /broker/envelopes` (v002-s05-04), there's no notification. Subscribers polling are the current fallback.

#### Fix direction

- In `internal/api/broker.go` (or a `internal/broker/service.go` layer): on envelope insert, publish `broker.envelope_created` with scope `"broker"`, `session_id` null, payload containing the envelope metadata (not full payload — a reference).
- On `POST /broker/envelopes/{id}/reply`: publish `broker.envelope_replied` with the target ID + correlation.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/broker.go`
- (or `/Users/chrispian/Projects-apps/agent-mux/internal/broker/service.go` if extracted)
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/kinds.go`

#### Acceptance criteria

- [x] POST envelope → subscriber with `scope=broker` receives event.
- [x] Reply → subscriber receives second event with matching correlation.

#### Test plan

- Unit: stubbed bus, handler test, assert event publish on insert.
- Integration: daemon + API, subscribe, POST envelope, verify event.

#### Scope fences

- Do not publish full envelope payloads in the event — just metadata (ID, sender, recipient, workflow_id, correlation_id, message_type). Clients fetch the payload separately if they need it.
- Do not implement request/reply blocking semantics here — that's Sprint v003-05.

#### Relationship

Depends on: T-v002-s03-04, T-v002-s05-04, T-v002-s06-01.

#### Origin

Context-pack [11-task-list-v0-0-2.md](../agent-mux-vfuture-context-pack/11-task-list-v0-0-2.md) milestone 6 ("publish broker events").

---

### T-v002-s06-04: Daemon lifecycle events

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [feature, events, daemon, observability]

#### Problem

Daemon-level events (startup, shutdown, config reload) are invisible to clients. Useful for Nanite/Clockwork to know when the daemon came up.

#### Fix direction

- Emit `daemon.started` with payload `{version, pid, listener_addrs}` at the end of daemon startup.
- Emit `daemon.shutdown_started` when Shutdown is called.
- Emit `daemon.shutdown_completed` when Shutdown finishes.

These events have `session_id` null, `scope: "daemon"`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/daemon.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/kinds.go`

#### Acceptance criteria

- [x] Starting the daemon emits `daemon.started`.
- [x] Stopping emits `daemon.shutdown_started` then `daemon.shutdown_completed`.
- [x] A subscriber with `scope=daemon` receives both.

#### Test plan

- Integration: start daemon with a subscriber connected, assert events arrive.

#### Scope fences

- Do not add config-reload events (no reload support in v0.0.2).
- Do not emit health-tick events — those are a separate observability concern (v0.1+).

#### Relationship

Depends on: T-v002-s01-02, T-v002-s06-01.

#### Origin

Context-pack [11-task-list-v0-0-2.md](../agent-mux-vfuture-context-pack/11-task-list-v0-0-2.md).

## Review / readiness notes

- **Event kind vocabulary:** pin the small set (session.state_changed, broker.envelope_created, broker.envelope_replied, daemon.started, daemon.shutdown_started, daemon.shutdown_completed). Resist adding more without clear use cases.
- **Payload schema per kind:** document in `docs/api/events.md` alongside the API reference. Clients depend on this.
- **Drop policy for slow subscribers** is a design call; drop-oldest with a counter is the safer default but surprises clients. Document clearly and surface drops in `daemon.*` events.
- **Event persistence overhead:** syncing every event to SQLite may become a hotspot at high frequency. Measure before optimizing; if needed, add an async batch writer in v0.1.
- **`stream_seq` interpretation (resolved in T-01):** The events table intentionally does not carry a separate `stream_seq` column — the existing `id INTEGER PRIMARY KEY AUTOINCREMENT` already provides a globally monotonic stream id. The bus uses `id` as `Event.Seq`; `Filter.SinceSeq` maps to `WHERE id > ?`. If Sprint v002-06 or beyond needs a per-scope sequence (e.g., per-session replay cursors), add it as a new column at that time rather than pre-adding an unused one. The exit-criteria line "stable stream_seq" is satisfied by AUTOINCREMENT's monotonic `id`.
- **`LogicalAgentID` on `Event` (resolved in T-01):** Carried on the in-memory `events.Event` for observability, but NOT persisted as a column. Emitters that need it to survive replay embed it in `PayloadJSON`. `Filter` intentionally omits a LogicalAgentID field in v0.0.2 — cross-session / per-agent filtering is deferred until a concrete use case emerges (e.g. Vanta memory views in v0.1+).
- **T-03 broker events and Sprint 5 dependency:** T-03 emits events on broker envelope create/reply. Per the sprint file's note ("or `internal/broker/service.go` if extracted"), we emit at the broker service layer rather than in an HTTP handler. Sprint 5's broker API wires the handler to the same service, inheriting event emission for free. This keeps T-03 unblocked by Sprint 5.
