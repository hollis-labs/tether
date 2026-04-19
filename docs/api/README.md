# Agent Mux Local API (v0.0.2)

The `muxd` daemon exposes an HTTP API for session lifecycle, attach streaming,
checkpoints, broker envelopes, and event observation. Clients are assumed to
run on the same host — there is no authentication. Trust is anchored to the
UDS filesystem permissions (or loopback interface for TCP transports).

## Transport

- Default: Unix domain socket at `~/.agent-mux/run/muxd.sock`.
- Alternate: TCP on loopback when `daemon.listen_addr` is `tcp:127.0.0.1:PORT`.

Over UDS, clients address the daemon with the `http://unix/<path>` convention;
the path component of the URL carries the API route.

## Common conventions

- All request/response bodies are JSON unless noted (attach is
  `application/octet-stream`, event stream is `text/event-stream`).
- Timestamps are RFC3339 UTC. Historical store queries emit RFC3339 with
  nanosecond precision.
- IDs are UUIDv7 for envelopes and checkpoints (time-sortable); sessions keep
  the v0.0.1 UUIDv4 form.

### Error envelope

Every non-2xx JSON response carries:

```json
{
  "error": {
    "code": "<machine code>",
    "message": "<human message>"
  }
}
```

Defined codes:

| Code                | HTTP | Meaning                                       |
|---------------------|------|-----------------------------------------------|
| `invalid_request`   | 400  | malformed body, missing required param        |
| `not_found`         | 404  | resource or action path doesn't exist         |
| `method_not_allowed`| 405  | route exists, method doesn't                  |
| `conflict`          | 409  | state precondition failed (e.g. wrong state)  |
| `payload_too_large` | 413  | body exceeded per-route cap                   |
| `not_implemented`   | 501  | route exists, semantics land in a later version |
| `internal_error`    | 500  | unexpected server failure                     |

---

## Health

### `GET /health`

Returns daemon liveness and basic telemetry.

Response:

```json
{
  "status": "ok",
  "pid": 12345,
  "uptime_sec": 42,
  "listener": "unix:/Users/me/.agent-mux/run/muxd.sock",
  "sessions": 2
}
```

---

## Sessions

Sessions move through `created → launching → running → {completed|failed|killed}`.
`POST /sessions` creates (state=created); `POST /sessions/{id}/launch` starts.

### `POST /sessions`

Create a session from a launch profile. Does not start the runtime.

Request body:

```json
{ "launch": "my-launch-id" }
```

Response (201):

```json
{
  "id": "88e1c18c-fca2-40a9-ac3a-ba25fd790869",
  "workspace": "/path/to/workspaces/proj/88e1c18c.../",
  "log": "/path/to/workspaces/proj/88e1c18c.../logs/session.log"
}
```

### `POST /sessions/{id}/launch`

Transition a `created` session to `running`. Returns 409 `conflict` when the
target is in any other state.

Response (200):

```json
{
  "id": "88e1c18c-...",
  "workspace": "/...",
  "log": "/..."
}
```

### `GET /sessions`

List sessions with optional filters.

| Query param | Type       | Description                                                   |
|-------------|------------|---------------------------------------------------------------|
| `limit`     | int 1–1000 | default 100                                                   |
| `cursor`    | RFC3339    | return rows strictly older than this created_at               |
| `state`     | string     | exact-match state filter                                      |

Response (200):

```json
{
  "sessions": [
    {
      "id": "88e1c18c-...",
      "launch_id": "my-launch",
      "project_id": "demo",
      "logical_agent_id": "demo-agent",
      "provider_id": "api-stub",
      "workspace": "/...",
      "state": "running",
      "pid": 0,
      "created_at": "2026-04-19T08:16:41Z",
      "updated_at": "2026-04-19T08:16:41Z",
      "attached_clients": 0
    }
  ],
  "next_cursor": "2026-04-19T08:14:10Z"
}
```

`next_cursor` is only emitted when the page filled the requested limit; pass
it back as `?cursor=` to continue.

### `GET /sessions/{id}`

Fetch a single session. 404 when not found.

Response (200): single `SessionDTO` as above.

### `POST /sessions/{id}/stop`

Signal the runtime to terminate the session. No body.

Response: 204 on success; 404 when the session is not currently running
(already exited or never registered).

### `GET /sessions/{id}/wait`

Long-poll until the session reaches a terminal state. Returns the exit code.

Response (200):

```json
{ "exit_code": 0 }
```

### `POST /sessions/{id}/input`

Write raw bytes to the session's PTY input. Request body is
`application/octet-stream` — no framing, no newline normalization. 1 MiB
per-request cap.

Response: 204 on success; 409 `conflict` when the session has no writable
input channel (e.g. api-stub runtimes).

### `GET /sessions/{id}/attach`

Live-stream the session's PTY output as an unframed byte stream
(`application/octet-stream`). Flushes after each chunk. Connection stays
open until the session exits or the client disconnects.

| Query param | Type  | Description                                                  |
|-------------|-------|--------------------------------------------------------------|
| `since_seq` | int64 | resume: replay bytes beyond this session-byte offset only    |

When `since_seq` is older than the oldest retained byte (ring eviction),
the handler silently replays the full ring — clients detect gaps by byte-
count comparison.

---

## Checkpoints

v0.0.2 ships persistence only. No runtime side effects: creating a
checkpoint does not pause the session or snapshot context. Resume is a
501 placeholder until v0.0.3 Sprint v003-04.

### `POST /sessions/{id}/checkpoint`

Create a checkpoint row bound to the session's logical agent. Body is an
optional free-form object; every field is optional.

Request body (example):

```json
{
  "task_id": "T-42",
  "workflow_id": "wf-1",
  "status": "in_progress",
  "completed_work": "parsed input",
  "pending_work": "write output",
  "key_decisions": "chose impl A over B",
  "referenced_artifacts": "file:///tmp/foo.md",
  "summary": "mid-task checkpoint",
  "next_recommendation": "continue on branch foo"
}
```

Response (201):

```json
{
  "id": "019da4d0-32b2-7318-a2ab-290a92d58fba",
  "logical_agent_id": "demo-agent",
  "summary": "mid-task checkpoint",
  "created_at": "2026-04-19T08:16:41Z",
  "source_session_id": "88e1c18c-..."
}
```

### `GET /logical-agents/{id}/checkpoints`

List checkpoints for a logical agent, newest first.

Response (200):

```json
{
  "checkpoints": [
    { "id": "...", "logical_agent_id": "demo-agent", ... }
  ]
}
```

### `POST /logical-agents/{id}/resume`

Placeholder: always returns 501 in v0.0.2 with the error envelope
`{"error":{"code":"not_implemented","message":"resume lands in v0.0.3"}}`.

---

## Broker envelopes

Envelopes are the mailbox-style inter-session message carrier. Write paths
emit `broker.envelope_created` / `broker.envelope_replied` events on the
bus; payloads are NOT leaked into events (metadata only).

### `POST /broker/envelopes`

Create and persist a new envelope. Server assigns `id` (UUIDv7) and
`created_at`; every other field round-trips.

Request body:

```json
{
  "sender": "alice",
  "recipient": "bob",
  "workflow_id": "wf-1",
  "correlation_id": "optional-seed",
  "message_type": "request",
  "priority": 0,
  "payload": "{\"op\":\"do-thing\"}",
  "audit_json": "{}"
}
```

Response (201): full `EnvelopeDTO`.

### `GET /broker/envelopes`

List envelopes by recipient OR workflow. Exactly one of `?recipient=` or
`?workflow_id=` is required; passing both is 400. `?correlation_id=` scopes
a workflow query further.

Query params:

| Param            | Description                                              |
|------------------|----------------------------------------------------------|
| `recipient`      | undelivered envelopes for this recipient, FIFO by created_at |
| `workflow_id`    | all envelopes in workflow, chronological                 |
| `correlation_id` | (with workflow_id) scope to one conversation thread      |

### `GET /broker/envelopes/{id}`

Fetch a single envelope. 404 when missing.

### `POST /broker/envelopes/{id}/reply`

Create a reply envelope: swaps `sender`/`recipient` from the original,
propagates `workflow_id`, and sets `correlation_id` to either the original's
correlation_id (if set) or the original's id (seeding a new thread).
Request body is an `EnvelopeCreateRequest`; its sender/recipient fields are
ignored, but `message_type`, `priority`, `payload`, `audit_json` carry
through.

Response (201): full reply `EnvelopeDTO`.

---

## Events

v0.0.2 has an in-memory pub/sub bus (Sprint v002-06) over session, daemon,
and broker scopes. Every event also persists to the `events` table.

### `GET /events/stream`

SSE stream of bus events. Replays history (via `since_seq`) then switches
to live.

| Query param  | Type  | Description                                                     |
|--------------|-------|-----------------------------------------------------------------|
| `since_seq`  | int64 | replay events with seq > N before live (0 = full replay)        |
| `scope`      | string (repeatable) | one of `session` / `daemon` / `broker`; filter      |
| `session_id` | string | restrict to a single session's events                          |

Frame format per event:

```
id: 42
event: session.state_changed
data: {"scope":"session","session_id":"abc","payload_json":"{...}"}

```

A blank line terminates each event. A keep-alive comment (`: ping`) fires
every 15s to keep proxies from closing idle connections.

Clients track `since_seq` themselves (typically the last seen `id:` field);
server does not persist subscription offsets.

### `GET /sessions/{id}/events`

Historical, paginated list of events for a single session. Newest first.

| Query param | Type       | Description                              |
|-------------|------------|------------------------------------------|
| `limit`     | int 1–1000 | default 100                              |
| `cursor`    | int64      | return rows with seq < cursor            |

Response (200):

```json
{
  "events": [
    {
      "seq": 42,
      "at": "2026-04-19T08:16:41.052136Z",
      "scope": "session",
      "session_id": "abc",
      "kind": "session.state_changed",
      "payload_json": "{\"from\":\"launching\",\"to\":\"running\"}"
    }
  ],
  "next_cursor": 38
}
```

`next_cursor` is emitted only when the page filled the limit.

---

## Event kinds

Current (v0.0.2):

| Scope    | Kind                          | Emitted by                             | Payload                                                      |
|----------|-------------------------------|----------------------------------------|--------------------------------------------------------------|
| daemon   | `daemon.started`              | muxd at listener-up                    | `{version, pid, listener}`                                   |
| daemon   | `daemon.shutdown_started`     | muxd on ctx cancel                     | empty                                                        |
| daemon   | `daemon.shutdown_completed`   | muxd after runtime drain, before Close | empty                                                        |
| session  | `session.state_changed`       | runtime.Manager at every transition    | `{from, to, exit_code?, reason?}`                            |
| broker   | `broker.envelope_created`     | broker.Service on successful persist   | `{id, sender, recipient, workflow_id, correlation_id, message_type}` — metadata only, never payload |
| broker   | `broker.envelope_replied`     | broker.Service on successful reply     | same shape as created                                        |

Additional kinds will land as v0.0.3 extends runtime and broker semantics.

---

## Versioning & stability

The routes documented here are stable for v0.0.2 — Nanite and Clockwork
adopt these shapes in v0.0.3 integration work. Adding new endpoints is
additive and non-breaking. Changing existing response shapes requires a
version bump at the mount-point level (not yet in scope).
