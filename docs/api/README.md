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
  "log": "/path/to/workspaces/proj/88e1c18c.../logs/session.log",
  "provider_id": "claude-stream"
}
```

`provider_id` resolves from the launch profile's referenced provider. Clients
use it to dispatch provider-kind-specific paths (chat surface vs. raw PTY
attach) without a follow-up `GET /sessions/{id}` round-trip.

### `POST /sessions/{id}/launch`

Transition a `created` session to `running`. Returns 409 `conflict` when the
target is in any other state.

Response (200):

```json
{
  "id": "88e1c18c-...",
  "workspace": "/...",
  "log": "/...",
  "provider_id": "claude-stream"
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

### `POST /sessions/{id}/resize`

Propagate a terminal resize to the session's PTY. Called by any attached
client whose terminal needs to keep the underlying PTY's window size in
sync (e.g. interactive shell wrappers around the attach stream).
See ADR 0014 for the full rationale.

```json
{"rows": 42, "cols": 120}
```

Response: 204 on success.

Errors: `invalid_request` (missing/zero rows/cols or malformed JSON);
`not_found` (session is not currently registered in the runtime — e.g.
already exited); `internal_error` (rare `pty.Setsize` failure).

Provider runtimes without a PTY (stub API provider) accept the call and
no-op.

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

## Catalog

Read-only list endpoints for the four catalog types. Writes (create /
update / delete / reload) are deliberately not exposed in v0.0.2 —
see [ADR 0012](../adr/0012-catalog-read-api.md) for the "reads now,
writes deferred" rationale.

Conventions (all four routes):

- Method: `GET` only. Other methods return `405 method_not_allowed`.
- Response: `{"<type>": [<record>, …]}` — the inner records are the
  catalog structs as-loaded from YAML (see
  `internal/config/model.go`). No pagination, no cursor — the catalog
  is small enough that clients pull the full list and filter locally.
- Records are sorted by `id` so repeated calls emit a stable order.
- Fresh reads per request: handlers re-read the catalog root on each
  call, so edits to the YAML files are picked up without a daemon
  restart.
- Errors: a missing or unreadable catalog root surfaces as `500
  internal_error` with a message citing the offending path (e.g.
  `"catalog load failed: load global: read /…/global.yaml: no such
  file or directory"`).

### `GET /catalog/projects`

Response (200):

```json
{
  "projects": [
    {
      "id": "demo",
      "name": "Demo Project",
      "repo_root": "~/Projects-apps/agent-mux-v0-pack",
      "tracking_root": "~/agent-mux/tracking/demo",
      "boot_fragments": ["boot/common.md"],
      "workspace": {
        "default_mode": "hybrid",
        "session_root": "~/agent-mux/workspaces/demo"
      }
    }
  ]
}
```

### `GET /catalog/agents`

Response (200):

```json
{
  "agents": [
    {
      "id": "demo-agent",
      "name": "Demo Agent",
      "roles": ["general"],
      "permissions": { "network": true, "default_sandbox": "none" }
    }
  ]
}
```

### `GET /catalog/providers`

Response (200):

```json
{
  "providers": [
    {
      "id": "api-stub",
      "type": "api",
      "command": "",
      "bootstrap": {},
      "env": { "mode": "merge" }
    }
  ]
}
```

### `GET /catalog/launches`

Response (200):

```json
{
  "launches": [
    {
      "id": "demo-launch",
      "project": "demo",
      "agent": "demo-agent",
      "provider": "claude-code",
      "workspace": { "mode": "hybrid" },
      "prompt": {
        "include_project_boot": true,
        "include_agent_boot": true,
        "include_knowledge_base": false
      },
      "overrides": {}
    }
  ]
}
```

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

## Registry

The federation directory service. Mux owns public-identity rows for
agents + projects (v060-01); substrates retain operational config behind
each row's `callback` URI. See [ADR 0041](adr/0041-registry-directory-service.md)
for the full rationale and [docs/registry/overview.md](../registry/overview.md)
for the integration guide.

The `{kind}` URL segment is **plural** (`agents`, `projects`); the
internal `Kind` value is singular (`agent`, `project`).

### `POST /registry/{kind}` — Register

Body: a Profile JSON. The caller supplies `display_name` and any optional
identity fields. The server assigns `urn`, `kind`, `created_at`,
`updated_at`, `mux_instance_id` and ignores any caller-supplied versions
of those.

Response: `201 Created` + the canonical Profile JSON (with the minted
URN).

```bash
curl -X POST http://unix/registry/agents -d @profile.json
```

### `GET /registry/{kind}` — Search

Query parameters (all optional, combine with AND):
- `role` — exact match on the `role` column
- `title` — exact match on `title`
- `project` — exact match on `project`
- `capability` — row has the capability in its `registry_capabilities`
- `skill_name` — row has a skill with this name
- `status` — `active` (default) | `deprecated` | `*` (all)

Response: `{"<kind-plural>": [Profile, ...]}` — alphabetical by
`display_name`. Empty result is `{"agents": []}`, never null.

```bash
curl 'http://unix/registry/agents?role=reviewer&status=active'
```

### `GET /registry/{kind}/{urn}` — Lookup

`{urn}` is URL-encoded (`msg%3A%2F%2Fagent%2Fagent-mux%2Fagt_xxx`).

Response: `200 OK` + Profile, or `404 not_found`. Soft-deleted rows are
still returned here with `status: "deprecated"`.

### `PATCH /registry/{kind}/{urn}` — UpdateSelf

Body: `UpdatePatch` JSON. Partial-merge semantics:
- Scalar fields use pointer-nil semantics (`null` or omitted = no change).
- Array fields (`capabilities`, `skills`, `links`) accept two shapes:
  - Shorthand: `"capabilities": ["a", "b"]` — equivalent to a REPLACE.
  - Explicit: `"capabilities": {"mode": "append"|"replace"|"remove", "value": [...]}`.
- Empty `value` is a no-op on every mode.
- `last_updated_by` is required.

```json
{
  "title": "Tether Sprint Implementer",
  "skills": {"mode": "append", "value": [{"name": "go-generics", "learned_at": "2026-05-20T00:00:00Z"}]},
  "last_updated_by": "chrispian@local"
}
```

### `DELETE /registry/{kind}/{urn}` — Deregister

Soft-delete; row's `status` flips to `deprecated`. Child rows
(capabilities/skills/links) are NOT touched. Response: `200 OK` + the
now-deprecated Profile.

### `POST /registry/{kind}/{urn}/sync` — Sync

Refreshes thin-profile columns from the row's `callback`. Raw payload is
never stored (D18). `file://` and `cli://` schemes ship in v1; `http://`
and `mcp://` land in v060-02.

- `204 No Content` if the row has no `callback`
- `200 OK` + refreshed Profile otherwise

### `POST /registry/bootstrap?force=true` — Re-run the catalog importer

Daemon already runs `BootstrapFromCatalog(force=false)` once at startup.
This endpoint lets operators apply catalog drift after editing a YAML by
re-running with `force=true` (refreshes existing rows from the source
YAML's current state). Body is empty; response is a `BootstrapReport`:

```json
{"imported": 0, "skipped": 30, "refreshed": 2, "errors": []}
```

### Error mapping

| Error | HTTP | Code |
|---|---|---|
| `registry.ErrInvalidRequest` | 400 | `invalid_request` |
| `registry.ErrNotFound` | 404 | `not_found` |
| `registry.ErrNoCallback` | 204 | (no body) |
| `registry.ErrNoResolver` | 400 | `invalid_request` |
| `registry.ErrPayloadInvalid` / `ErrPayloadTooLarge` / `ErrPathOutsideRoot` | 502 | `internal_error` |
| `registry.ErrMintExhausted` | 503 | `internal_error` |
| unsupported kind segment | 404 | `not_found` |
| method not allowed | 405 | `method_not_allowed` |
| other | 500 | `internal_error` |

---

## Groups

Group messaging — a `group` registry kind with mailbox-pull delivery
semantics, per-member read cursor, and server-side `@` mention parsing.
See `docs/adr/0042-group-messaging.md` for the architectural decision
and `docs/groups/symbols.md` for the `@` / `!` / `:` vocabulary.

| Route | Method | Description |
|-------|--------|-------------|
| `/groups` | `POST` | Create a group. Body: `{display_name, description?, role?, capabilities?, avatar?, last_updated_by}`. The caller URN goes in `last_updated_by`; it's auto-added to `group_members` with `role='owner'` inside the same transaction as the profile insert. Returns the reloaded `Profile`. |
| `/groups?member=<urn>` | `GET` | List groups that `<urn>` belongs to, ordered alphabetically by `display_name`. Status is not filtered (archived groups appear). |
| `/groups/{urn}` | `GET` | Read a group profile by URN. URN is path-escaped (e.g. `msg%3A%2F%2Fgroup%2Fagent-mux%2Fgrp_x9k2p4`). |
| `/groups/{urn}` | `DELETE` | Archive a group (soft — sets `status='archived'`, read-only). Body: `{"by": "<urn>"}` (or `?as=<urn>`). Owner/moderator only. |
| `/groups/{urn}/members` | `POST` | Add a member. Body: `{"member": "<urn>", "by": "<urn>", "role"?: "member|moderator|owner"}`. Role defaults to `member`. Owner/moderator only. |
| `/groups/{urn}/members` | `GET` | List members of a group, ordered by `joined_at` ASC, with `display_name` hydrated. |
| `/groups/{urn}/members/{member_urn}` | `DELETE` | Remove a member. Body: `{"by": "<urn>"}` (or `?as=<urn>`). Owner/moderator only; cannot remove the owner. |
| `/groups/{urn}/members/{member_urn}` | `PATCH` | Change a member's role. Body: `{"role": "...", "by": "<urn>"}`. Promotion to `owner` is owner-only. |
| `/groups/{urn}/leave` | `POST` | Self-leave path. Body: `{"member": "<urn>"}`. If the leaver is the owner and no other owner/moderator exists, the leave is refused. |
| `/groups/{urn}/messages` | `POST` | Send a message to the group. Body: `{"from": "<urn>", "kind": "...", "payload"?: ..., "thread_id"?: "...", "content_type"?: "..."}`. Returns `{message_id, group_seq}`. Non-member → 403; archived group → 423 Locked. Mention parser runs server-side: ambiguous `@`-short-form → 400 `invalid_request` with the `candidates` array. |
| `/groups/{urn}/messages?since_seq=N&thread_id=...&limit=N&as=<urn>` | `GET` | List messages addressed to the group with `group_seq > since_seq`. `as` is the requesting-member URN; required for membership + joined_at gate. `since_seq=0` defaults to the member's `last_read_seq`. Does NOT bump the cursor. |
| `/groups/{urn}/read` | `POST` | Mark messages read. Body: `{"up_to_seq": N, "as": "<urn>"}`. Monotonic — smaller `up_to_seq` is a no-op. |
| `/mentions?as=<urn>&since=<ts>&limit=N` | `GET` | List the caller's mention notices (notice envelopes with `payload.group` set), newest first. |

### Caller identity (v1)

Until v060-03 token auth lands, the caller URN is supplied per-request via
one of these channels (the handler picks whichever fits the verb's
semantic — body wins when both body and query are supplied):

- **Body fields:**
  - `last_updated_by` on `POST /groups` (creator URN, becomes owner).
  - `by` on moderation actions (archive / add-member / remove-member /
    set-role) — the actor.
  - `member` on `POST /groups/{urn}/leave` — the self-leaver.
  - `from` on `POST /groups/{urn}/messages` — the message author.
  - `as` on `POST /groups/{urn}/read` — the member whose cursor moves.
- **Query fallback:** `?as=<urn>` on GET / DELETE verbs where building a
  body is awkward (the `mux` CLI uses this).

The HTTP layer does not authenticate the URN — same-host UDS trust per
ADR-0041 §D7.

### Symbol vocabulary

The daemon parses `@` mentions in `POST /groups/{urn}/messages` payloads
and emits notice envelopes to mentioned URNs' personal inboxes. `!` and
`:` are reserved-namespace for agent-side handling — the daemon
transports them verbatim. See `docs/groups/symbols.md` for the full
reference; `tether_group_post` MCP tool description embeds the same
distinction inline so agent authors see it at the tool level.

### Error mapping

| Sentinel | HTTP | Envelope code |
|----------|------|---------------|
| `registry.ErrInvalidRequest` | 400 | `invalid_request` |
| `*registry.ErrAmbiguousMention` | 400 | `invalid_request` (with `candidates` array in the response payload) |
| `registry.ErrForbidden` | 403 | `forbidden` |
| `registry.ErrNotFound` | 404 | `not_found` |
| `registry.ErrGroupArchived` | 423 | `locked` |
| method not allowed | 405 | `method_not_allowed` |
| other | 500 | `internal_error` |

### Known v1 limitations

- Display-name ambiguity blocks the `@<short-form>` mention path. If
  two agents share `display_name`, `SendToGroup` aborts with 400 + the
  candidate URN list; the caller must use the full URN.
- Group URNs are not yet recognized by go-messaging v0.2.1's
  `AddressKind` enum, so the existing `/messages/*` routes reject them
  at `ParseURN`. Group reads go through `/groups/{urn}/messages`.
  Cross-substrate group routing via the ADR-0040 federation Router
  works structurally (3-segment URN preserves the authority segment)
  but waits on a go-messaging bump for end-to-end correctness.
- v1 has no private-membership model — member lists are visible to all
  members.
- No event emission on group writes — consumers re-pull on cache miss
  (same scope-fence as the registry's other surfaces).

---

## Versioning & stability

The routes documented here are stable for v0.0.2 — Nanite and Clockwork
adopt these shapes in v0.0.3 integration work. Adding new endpoints is
additive and non-breaking. Changing existing response shapes requires a
version bump at the mount-point level (not yet in scope).
