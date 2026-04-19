# internal/broker

Broker envelope service — point-to-point messaging between logical
agents, with reply + correlation support.

**Purpose:** persists `BrokerEnvelope` rows, emits lifecycle events on
the event bus, and exposes a `ReplyEnvelope` convenience that swaps
sender/recipient + propagates `workflow_id` + `correlation_id`.

**Entry points:**

- `Envelope` struct (`model.go`) — mirrors the `broker_envelopes` table.
- `Service` (`service.go`) — `NewService(store, publisher)` → methods:
  - `CreateEnvelope` — persist and emit `broker.envelope_created`.
  - `ReplyEnvelope` — derive a reply from an existing envelope.
- On each write, the bus receives a `broker.envelope_created` or
  `broker.envelope_replied` event. Payload is metadata only (id,
  sender, recipient, workflow_id, correlation_id, message_type) —
  bodies do not leak into events.

**Neighbors:**

- Store CRUD in `internal/store/broker_envelopes.go`.
- Event publishing via [`internal/events`](../events).
- HTTP surface in [`internal/api`](../api) (broker endpoints) consumes
  this service.

**Gotchas:**

- IDs are UUIDv7 (sortable). Enforced at the API layer, not here.
- `correlation_id` on a reply defaults to the original envelope's
  `correlation_id` (or its `id` when the original had none).
- Persist-before-emit: a store failure does not publish. Do not move
  the `Publish` call before the `store.Insert` call.
