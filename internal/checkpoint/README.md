# internal/checkpoint

Checkpoint types — snapshots of session state that a resume flow can
later rehydrate.

**Purpose:** describes the persisted shape of a checkpoint row. v0.0.2
only stores and retrieves checkpoints; resume semantics land in v0.0.3
Sprint v003-04.

**Entry points:**

- `Checkpoint` struct (`model.go`) — mirrors the `checkpoints` table
  (id, logical_agent_id, source_session_id, workflow_id, hints_json,
  created_at).

**Neighbors:**

- Written + read by [`internal/store`](../store) (CRUD in
  `store/checkpoints.go`).
- Exposed via HTTP by [`internal/api`](../api) — skeletal POST/GET
  endpoints land in Sprint v002-05; resume (501) lands in v0.0.3.
- `CheckpointHint` from [`internal/provider`](../provider) feeds into
  the hints JSON payload.

**Gotchas:**

- Types-only. No logic. Don't add business code here; add it to `store`
  (persistence) or `api` (surface) instead.
- `hints_json` is deliberately opaque until v0.0.3 pins the schema.
  Treat it as a blob.
- IDs are server-assigned UUIDv7 at the API layer (sortable,
  time-ordered) — see [`internal/api`](../api).
