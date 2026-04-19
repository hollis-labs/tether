# internal/events

In-memory pub/sub event bus with store-backed persistence.

**Purpose:** lifecycle events from the runtime manager, daemon server,
and broker service flow through a single `Bus`. Subscribers receive a
filtered, replay-then-live stream; every event is persisted so late
subscribers can catch up via `since_seq`.

**Entry points:**

- `Event` (`model.go`) — Scope / SessionID / LogicalAgentID / Kind /
  PayloadJSON / Seq / At.
- `Scope` constants + `Kind*` constants (`kinds.go`).
- `Bus` (`bus.go`) — `NewBus(BusOptions{Persister})`; `Publish` +
  `Subscribe` (returns `<-chan Event`, cancel, err). Drop-oldest on
  full subscriber; `MaxConsecDrops` evicts chronic slow consumers.
- `Publisher` interface — the write-half, extracted so runtime and
  daemon can depend on just that.
- `Filter` (`filter.go`) — scopes allow-list, session_id, since_seq.
- `Persister` interface (`persist.go`) — satisfied by `*store.Store`.

**Neighbors:**

- [`internal/runtime`](../runtime) emits `session.state_changed`.
- [`internal/daemon`](../daemon) emits `daemon.started` /
  `daemon.shutdown_started` / `daemon.shutdown_completed`.
- [`internal/broker`](../broker) emits envelope lifecycle events.
- [`internal/api`](../api) exposes `GET /events/stream` (SSE) + per-session
  historical list.
- [`internal/store`](../store) persists every published event via
  `Persister.InsertEvent`; stream_seq is the table's
  `AUTOINCREMENT` id.

**Gotchas:**

- Drop-oldest is intentional: slow consumers must not block the runtime.
  The bus tolerates `MaxConsecDrops` drops per subscriber before
  evicting them (closes the user channel, deregisters).
- Replay-then-live ordering is guaranteed via an internal `done`
  channel — never close the live channel while producing (race under
  `-race`).
- `SinceSeq=0` is "full replay", not "empty replay". Callers wanting
  live-only must pass a very large SinceSeq (or the current head).
- Emit-after-close is silently lost because Close tears down the
  underlying store-backed persister. `daemon.shutdown_completed` is
  emitted *after* manager drain but *before* the server's
  `Close()` — see `internal/daemon/server.go`.
