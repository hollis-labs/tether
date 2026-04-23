# ADR-0023 — Message Routing Contract

**Status:** Accepted
**Date:** 2026-04-22
**Supersedes:** —
**Extends:** ADR-0018 (broker envelope types), ADR-0022 (Mux as optional provider substrate)

---

## Context

Mux's high-leverage role is **message routing** — a reliable, provider-neutral,
client-neutral channel for agent-to-agent and agent-to-user communication.
Clockwork may use Mux message routing between its orchestrators, workers, and
reviewers when it chooses Mux as its session/provider path. Nanite may use the
same routing surface from its plugin. Other consumers (dashboard, CLI clients)
reconnect and query missed messages.

Without a formal contract:

- Producers guess at address shapes, kinds, and lifecycle rules.
- Consumers disagree on what "delivered" vs. "consumed" means.
- Concurrent consumers can double-deliver or permanently lose messages.
- Reconnecting clients have no reliable way to catch up.
- Error semantics differ between the HTTP API and MCP tools.

This ADR defines the single canonical contract that all producers and
consumers of Mux message routing must follow. It describes the address
scheme, message kinds, request/reply correlation, inbox/thread behavior,
consume/cancel semantics, durability expectations, and explicit failure cases.

---

## Decision

### 1. Address URNs

Every sender and recipient is identified by a canonical URN:

```
msg://<kind>/<authority>/<id>[/<subid>]
```

| Segment     | Definition                                                       |
|-------------|------------------------------------------------------------------|
| `kind`      | `agent`, `user`, `service`, `session`, `workflow`               |
| `authority` | Namespace / owner (e.g. `agent-mux`, `clockwork`, `nanite`)     |
| `id`        | Stable entity identifier                                         |
| `subid`     | Optional sub-entity (e.g. session ID within an agent identity)  |

**Conventional addresses:**

| Entity                         | URN                                               |
|-------------------------------|---------------------------------------------------|
| User "chrispian"               | `msg://user/agent-mux/chrispian`                  |
| Orchestrator agent             | `msg://agent/agent-mux/orchestrator`              |
| Planner agent                  | `msg://agent/agent-mux/planner`                   |
| Worker agent (any)             | `msg://agent/agent-mux/worker`                    |
| Reviewer agent                 | `msg://agent/agent-mux/reviewer`                  |
| Nanite plugin (session-scoped) | `msg://agent/nanite/plugin/<session-id>`          |
| Clockwork worker task          | `msg://service/clockwork/<task-id>`               |

**Validation:** `ParseURN` (`go-messaging` library) rejects unknown `kind`
values or malformed structure. Invalid URNs are rejected at write time with
`invalid_request`.

---

### 2. Message Kinds

The `kind` field is a closed enum. Mux does not interpret kind beyond routing;
semantics belong to the application layer.

| Kind            | Direction       | Semantics                                                        |
|-----------------|-----------------|------------------------------------------------------------------|
| `request`       | sender → target | Initiates a work item or question. Expects a `reply`.           |
| `reply`         | target → sender | Carries the answer to a `request`. Sets `in_reply_to`.          |
| `notification`  | sender → target | One-way informational. No reply expected.                        |
| `handoff`       | agent → agent   | Transfers ownership of a work unit across logical agents.        |
| `status_update` | agent → any     | Periodic progress report from an agent.                          |
| `escalation`    | agent → user    | Signals that human or elevated-priority attention is needed.     |

**Wire names** match the `go-messaging` `Kind` constants (`MsgKindRequest`,
`MsgKindResponse` → stored as `"request"` / `"reply"` etc.). See
`internal/mcpadapter/messages.go` and `go-messaging/messaging.go` for the
canonical enum.

> Note: `go-messaging` v0.2 uses `MsgKindResponse` for the "reply" kind; the
> wire string is `"response"`. The MCP tool descriptions and API docs use
> `"reply"` as the human term but `"response"` on the wire. ADR-0018 constants
> are `TypeResponse` / `TypeRequest` for the legacy broker surface. Both
> surfaces are compatible; the `messages` table uses the go-messaging wire
> strings exclusively.

---

### 3. Request / Reply Correlation

When a sender wants a synchronous or correlated exchange:

1. Sender calls `POST /messages` (or `mux_message_send`) with `kind=request`.
   The Store assigns `id` (UUIDv7).
2. The recipient receives the request (via `Inbox` or `Subscribe`).
3. The recipient calls `POST /messages` with `kind=response` (or uses
   `Dispatcher.Reply`) setting `in_reply_to=<request.id>` and
   `thread_id=<same thread>`.
4. If the sender used `POST /messages/request` (the blocking endpoint), the
   `Dispatcher` unblocks and returns the reply within the timeout window.

**Correlation rules:**
- `in_reply_to` MUST be the `id` of the originating envelope (not its own
  correlation ID). This is a stable cross-hop reference.
- `thread_id` is caller-assigned and used to group related exchanges. Any
  message in a conversation should carry the same `thread_id`.
- The server does **not** validate that `in_reply_to` references an existing
  envelope (deferred; see Consequences). Callers are responsible.
- Multiple replies to the same request are permitted (streaming result
  pattern). The blocking `Dispatcher.Request` returns on the **first** reply.

---

### 4. Inbox Semantics

`GET /messages/inbox?to=<urn>` (or `mux_message_inbox`) returns **undelivered**
messages for the given recipient, ordered chronologically (CreatedAt ASC, ID
ASC as tiebreaker).

**Atomic delivery side effect:** Every message returned by `Inbox` has
`delivered_at` set atomically to `now`. A second `Inbox` call for the same
recipient will **not** return those messages again. This is idempotent from
the consumer's perspective: once delivered, a message is out of the inbox.

**Deliver ≠ Consume.** Delivered means the recipient has seen the message.
Consumed means the recipient has finished processing it. Producers
that care about processing acknowledgment should use `Consume`.

**Reconnect / catch-up:** A client that restarts can call `Inbox` again; it
will receive any messages that arrived while disconnected (not yet delivered).
Messages that were already delivered but not consumed are available via
`GET /messages/{id}` by ID.

**Filter params:**
- `kind=request,notification` — comma-separated kind filter (OR semantics).
- `thread_id=<id>` — scope to a single thread.
- `limit=N` (default 100, max controlled by store).

---

### 5. Thread Semantics

`GET /messages/thread/{thread_id}` (or `mux_message_thread`) returns **all**
messages sharing a `thread_id`, chronologically. This is **read-only** — no
`delivered_at` side effects. Use it to reconstruct the full conversation
history without consuming messages.

---

### 6. Consume Semantics

`POST /messages/{id}/consume?as=<urn>` (or `mux_message_consume`) sets
`consumed_at` for the `(id, recipient)` pair.

- **Idempotent:** calling Consume on an already-consumed message is a no-op
  (returns success).
- **Recipient-scoped:** only the intended recipient can mark a message consumed.
  The `as` URN is validated against the envelope's `to_urn`.
- **Not a delete:** consumed messages remain in the store and are queryable
  by ID. They no longer appear in `Inbox`.

Consumers SHOULD call `Consume` when they have durably processed the message
(e.g., task dispatched, checkpoint emitted, reply sent). This enables producers
to monitor completion via `GET /messages/{id}`.

---

### 7. Cancel Semantics

`POST /messages/{id}/cancel` (or `mux_message_cancel`) marks a message as
dead. Use when:
- A sender dispatched a request that is no longer meaningful (e.g., the task
  was aborted before the worker replied).
- A request timed out and the sender wants to retire it.

**Rules:**
- Idempotent: canceling a canceled message is a no-op.
- `ErrNotFound` if the ID does not exist.
- Canceled messages disappear from `Inbox` (the `delivered_at IS NULL`
  filter excludes `canceled_at IS NOT NULL` messages — see migration 0010).
- Any `Dispatcher.Request` blocking on `InReplyTo=<id>` resolves with
  `ErrCanceled` (in-memory only; the Store does not persist this signal,
  but `notifyCanceled` can be wired to unblock subscribe waiters if needed).

---

### 8. Subscribe / SSE Live Stream

`GET /messages/subscribe?to=<urn>` opens an SSE stream of envelopes
delivered to `to` from the subscription point forward. No historical replay —
use `Inbox` for that.

**Semantics:**
- Envelopes appear on the stream as soon as `Send` completes.
- Filter params: `kind=`, `thread_id=` (same as Inbox).
- A `: ping` comment is emitted every 15 seconds to keep proxies alive.
- Closing the connection unregisters the subscription; the channel closes
  cleanly.
- Fan-out is **in-memory only**: subscriptions are lost on daemon restart.
  Reconnecting clients must call `Inbox` first to catch up, then subscribe.

---

### 9. Durability Expectations

| Layer             | Guarantee                                                           |
|-------------------|---------------------------------------------------------------------|
| Store (SQLite)    | Durable: survives daemon restart. All `messages` rows are WAL-synced. |
| Subscribe fan-out | Ephemeral: in-memory only; not replayed on reconnect.              |
| Delivered / Consumed state | Durable: persisted in `delivered_at` / `consumed_at` columns. |
| Canceled state    | Durable: `canceled_at` column persisted.                          |

Mux **does not guarantee exactly-once delivery** to subscribers. A message
sent during a subscriber absence will be replayed via `Inbox` on the next
call. Applications that need exactly-once processing must implement their
own idempotency (e.g., track consumed IDs externally).

---

### 10. Idempotency / Duplicate Handling

- `Send` always creates a new row (new UUIDv7 `id`). There is no
  deduplication key. Callers that need deduplication should include a
  correlation key in `metadata` and query before sending.
- `Consume` is idempotent (see §6).
- `Cancel` is idempotent (see §7).
- `Inbox` is NOT idempotent: each call advances `delivered_at` for returned
  messages. Callers MUST NOT re-query `Inbox` for the same recipient
  concurrently without external coordination.

---

### 11. Ordering Guarantees

Within a single `to_urn` + `delivered_at IS NULL` window:
- `Inbox` returns messages in `(created_at ASC, id ASC)` order.
- `id` is a UUIDv7, monotonically increasing within the same millisecond.
- Across different senders or daemon restarts, ordering is clock-dependent
  and not strictly causal. Use `thread_id` to reconstruct causal chains.

---

### 12. Visibility / Query Rules

| Query                     | Returns                                                     |
|---------------------------|-------------------------------------------------------------|
| `Inbox`                   | Undelivered, uncanceled messages for recipient              |
| `Thread`                  | All messages in thread (any lifecycle state)                |
| `Get /messages/{id}`      | Any message by ID (including delivered/consumed/canceled)   |
| `Subscribe`               | Live-only new messages addressed to recipient               |

---

### 13. Error Cases

| Error               | Trigger                                                     | Response          |
|---------------------|-------------------------------------------------------------|-------------------|
| `invalid_request`   | Missing `from`, `to`, `kind`; malformed URN; invalid kind  | HTTP 400          |
| `not_found`         | `Get` / `Cancel` / `Consume` on nonexistent `id`           | HTTP 404          |
| `ErrNotFound`       | Same, from Store layer                                      | Sentinel error    |
| `ErrPresetLifecycle`| Caller sets `delivered_at` or `consumed_at` on `Send`      | HTTP 400          |
| `ErrRequestTimeout` | `POST /messages/request` timeout elapsed                   | HTTP 504          |
| `ErrCanceled`       | `Dispatcher.Request` resolves on canceled request           | Store error       |
| `internal_error`    | Unexpected store failure                                    | HTTP 500          |

---

### 14. Concrete Flows

#### A. Clockwork orchestrator dispatches work to a Mux-backed worker

```
Orchestrator (msg://agent/agent-mux/orchestrator)
  POST /messages  { kind: "request", to: "msg://agent/agent-mux/worker",
                    thread_id: "T-abc", payload: { task_id: "CW-123" } }
  → id: "01JQ..."

Worker (msg://agent/agent-mux/worker)
  GET /messages/inbox?to=msg://agent/agent-mux/worker
  → [{ id: "01JQ...", kind: "request", ... }]
  POST /messages/01JQ.../consume?as=msg://agent/agent-mux/worker
  ... executes task ...
  POST /messages  { kind: "response", to: "msg://agent/agent-mux/orchestrator",
                    in_reply_to: "01JQ...", thread_id: "T-abc",
                    payload: { status: "done" } }
```

#### B. Worker replies, orchestrator unblocks via blocking endpoint

```
Orchestrator:
  POST /messages/request?timeout=30s
    { kind: "request", to: "msg://agent/agent-mux/worker", payload: { ... } }
  ← blocks ←

Worker: (sees via Subscribe)
  Dispatcher.Reply(ctx, requestEnv, payload)

Orchestrator: ← unblocks, receives reply envelope
```

#### C. Reviewer receives a handoff

```
Worker → POST /messages
  { kind: "handoff", to: "msg://agent/agent-mux/reviewer",
    thread_id: "T-xyz", payload: { task_id: "...", artifact: "..." } }

Reviewer → GET /messages/inbox?to=msg://agent/agent-mux/reviewer&kind=handoff
  → [handoff envelope]
  POST /messages/{id}/consume?as=msg://agent/agent-mux/reviewer
```

#### D. Nanite plugin sends a notification to a session's user

```
Nanite plugin (msg://agent/nanite/plugin/<session-id>)
  POST /messages
    { kind: "notification", to: "msg://user/agent-mux/chrispian",
      payload: { text: "Session X started successfully" } }

User client:
  GET /messages/inbox?to=msg://user/agent-mux/chrispian
  → [notification]
```

#### E. Client reconnects and queries missed messages

```
Client reconnects after crash:
  1. GET /messages/inbox?to=<my-urn>      ← gets all undelivered-since-crash
  2. GET /messages/subscribe?to=<my-urn>  ← live stream from this point
```

---

## Rationale

**Why is delivery a side effect of Inbox?**
The Inbox-delivery coupling (from `go-messaging` v0.2 contract) prevents
repeated deliveries without requiring the consumer to call a separate
"mark delivered" endpoint. It trades simplicity for idempotency risk on the
read side. The Consume step covers the "I've actually processed this" signal.

**Why no server-side deduplication on Send?**
Mux is a router, not a message bus. Deduplication policy belongs to the
application. Forcing a deduplication key on every send would impose
application-level semantics onto an infrastructure-level primitive.

**Why in-memory fan-out only?**
Durable push delivery (e.g. persistent subscriptions, at-least-once push)
would require a worker loop, message visibility timeouts, and retry state —
i.e., a queue broker. Mux stores messages durably; consumers are responsible
for polling `Inbox` on reconnect. This keeps the implementation minimal while
enabling all required flows.

**Why not encode Clockwork sprint/task policy in Mux messages?**
Mux routes envelopes. Clockwork owns its task lifecycle. Encoding
Clockwork semantics in Mux fields (e.g., `task_id` as a first-class URN
segment) would couple the two systems in ways that break when either
evolves. The `payload` field carries application-specific data; Mux
ignores it.

---

## Consequences

### Implementation pointers

| Package/File                              | Responsibility                                          |
|-------------------------------------------|---------------------------------------------------------|
| `go-messaging/messaging.go`               | Envelope, Filter, Kind, Address types                   |
| `go-messaging/store.go`                   | Store + Dispatcher interface contract                   |
| `go-messaging/messagingtest/contract.go`  | Behavioral contract test suite                          |
| `internal/store/messaging_store.go`       | SQLite-backed Store impl                                |
| `internal/api/messages.go`               | HTTP handlers — Send, Inbox, Thread, Consume, Cancel, Subscribe, Request |
| `internal/mcpadapter/messages.go`        | MCP tools — same operations surfaced for agents          |
| `internal/store/sqlite.go`              | `MessagingStore()` singleton (msgOnce)                  |
| `docs/db/migrations/0010_messages.sql`   | Schema: `messages` table + indexes                       |

### Implemented hardening (CW-20260423-0020)

- ✅ **Consume returns 404 when message not found** — `store.Consume` now
  checks rows-affected and queries existence; HTTP handler maps `ErrNotFound`
  to 404, `store.ErrWrongRecipient` to 409.
- ✅ **Consume returns 409 when caller `as` URN ≠ `to_urn`** — new
  `store.ErrWrongRecipient` sentinel; HTTP and MCP handlers both handle it.
- ✅ **Cancel returns 404 when not found** — already correct; verified.
- ✅ **Send validates `kind`** against the closed enum; returns `invalid_request`
  on unknown kinds. Added to both HTTP handler and MCP `mux_message_send`.
- ✅ **Inbox `canceled_at IS NULL` guard** confirmed in schema.
- ✅ **MCP `mux_message_consume`** now returns `conflict` for wrong-recipient.
- ✅ **MCP `mux_message_send` kind enum validated** — returns `invalid_request`.
- ✅ **SQLite WAL mode + busy_timeout** — `store.Open` now sets
  `journal_mode=WAL` and `busy_timeout=5000` and `MaxOpenConns=1` to prevent
  `SQLITE_BUSY` under concurrent access.
- ✅ **Inbox atomic delivery transaction** — `Inbox` now runs inside a
  `BEGIN`…`COMMIT` transaction so the SELECT + UPDATE are atomic, preventing
  concurrent callers from double-delivering the same messages.

### Implemented tests (CW-20260423-0021)

- ✅ Concurrent `Inbox` calls for same recipient — each message delivered exactly once.
- ✅ Concurrent `Consume` calls — idempotent; ConsumedAt set exactly once.
- ✅ Concurrent `Cancel` + `Consume` — no panic, no opaque errors.
- ✅ Concurrent `Send` + `Subscribe` — all messages arrive; no drops under fanOut lock.
- ✅ `Cancel` + in-flight subscribe wait — resolves without deadlock (liveness).
- ✅ Concurrent `Dispatcher.Request` + Reply pairs — correct correlation, no cross-contamination.

All tests pass with `-race` flag: `go test -race ./internal/store/... ./internal/api/... ./internal/mcpadapter/...`

### Open questions / deferred

- **Validate `in_reply_to` references an existing request** — deferred per
  ADR-0018. Can be added once the dispatcher's request registry is live.
- **Per-host ACLs** — a sender URN does not need to match any session identity.
  Deferred to v0.1 security hardening.
- **Handoff routing to human queue / Clockwork** — `kind=handoff` is accepted
  and stored; no special routing logic yet.
- **Persistent subscriptions / durable push** — requires a queue broker
  model. Out of scope for v0.
- **`notifyCanceled` fan-out** — currently a no-op in `messaging_store.go`.
  When `Dispatcher.Request` is wired to the SQLite store (vs. memstore),
  Cancel should deliver a sentinel to unblock in-flight waiters.

---

## References

- ADR-0018 — Broker Envelope Types and Correlation-ID Scheme
- ADR-0022 — Mux as Optional Provider/Session Substrate
- `github.com/hollis-labs/go-messaging` — messaging library and contract tests
- CW-20260423-0019 — this spec
- CW-20260423-0020 — implementation hardening
- CW-20260423-0021 — race/concurrency tests
