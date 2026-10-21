# Managed-session snapshot and event contract

Status: **DRAFT v1** for CW-20261021-0001 (managed-agent-capacity workstream).
Consumers: the Tachyon session (capacity view). Reviewer of the contract: Portfolio Architect.
Anything not marked otherwise is read from the code on `main` (`524bcd5a`); nothing here is invented.

Every field and event below carries one of three marks:

- **SHIPPED**: emitted today, exactly as written.
- **PROPOSED**: part of this contract, implemented by the named task. Not available until that task is deployed. Clients must not rely on it before then.
- **NOT POPULATED**: named so nobody builds on it. Unknown is `null`/absent, never a default.

Contract changes are additive. A breaking change bumps `Tether-Protocol` (currently `1`). A deviation from this document is announced to the Tachyon session before it ships.

## 1. Identity

The **session UUID is the only join key** (`sessions.id`, `session_id` on events, `id` on a session item). Nothing else (logical agent id, launch id, worker id, pid, provider session id, URN) identifies a session in this contract, and none of them is unique per session.

`instance_id` (`Session.InstanceID`) is **NOT POPULATED** and is **dropped from this contract**: the field is omitted from every payload (not `null`, not an invented value). If instance identity is ever needed it is a new, announced field.

## 2. Routes and access

| Route | Method | Scope | Answer |
|---|---|---|---|
| `/environment/snapshot` | GET | `tether.read` | environment snapshot, all sessions |
| `/sessions/{id}/snapshot` | GET | `tether.read` | the same shape with exactly one session; 404 if unknown |
| `/environment/events?after_seq=N` | GET (SSE) | `tether.read` | event stream |
| `/sessions/{id}/stream?after_seq=N` | GET (SSE) | `tether.read` | the same stream for one session |

On the remote listener every request carries `Tether-Protocol: 1`. A paired device uses its bearer credential. **Capabilities are not authorization** (section 6): holding `tether.read` does not mean a session supports an operation, and a supported operation does not mean the caller may perform it.

## 3. Cursor and recovery (SHIPPED)

- `seq` is the **environment-global** durable event id (`events.id`). It is one monotonically increasing sequence for the whole environment. Filtering, a session path, or a view never renumbers it.
- A snapshot carries `high_water_seq` (the cursor the snapshot is consistent with, read in the same SQLite read transaction as the session rows) and `earliest_available` (the oldest retained `seq`).
- **Resume rule:** take a snapshot, apply it, then open the stream with `after_seq=<high_water_seq>` (or `Last-Event-ID: <seq>`; `after_seq` wins). `after_seq` is required: without it the stream answers `400 after_seq required; take a snapshot first`.
- The stream registers for live wake-ups **before** reading the durable catch-up, so nothing is lost between them. Every delivered event is read from the durable log; a live notification is only a hint.
- SSE frames: `id: <seq>`, `event: <kind>`, `data: <json>`. Two control frames exist:
  - `event: synchronized`, `data: {"environment_id","seq"}`: sent after every drain, at the cursor the server has reached. **A client that sees it may advance its stored resume cursor to `seq`** even if no data event was delivered (this is what makes a filtered stream resumable).
  - `event: gap`, `id: <after_seq>`, `data: {"environment_id","reason","earliest_available","high_water_seq","snapshot_required","retry_after_seconds"?}`: the server cannot continue from the cursor and **closes the stream**. The cursor is not advanced. `snapshot_required: true` means take a new snapshot and resume from its `high_water_seq`.
- `gap` reasons: `ahead` (cursor is beyond the head), `purged` (history before `earliest_available`, or an interior hole, was retained away), `too_large` (more than the catch-up window, 2048 events, is missing), `restart` (the server could not map an event or its live feed ended), `event_id_invariant` (a stored row has no usable event id), `backfill_in_progress` (not a snapshot case: `snapshot_required: false`, retry after `retry_after_seconds`).
- Duplicate delivery is possible across reconnects. `seq` is the dedupe key; applying an event with `seq <= last_applied` is a no-op.
- The server also re-reads the durable log on a timer (default 1 s) as a safety net next to the live wake-ups. Its disposition is CW-20261021-0005; clients add **no polling** of their own.

## 4. Event envelope (SHIPPED)

One JSON object per SSE `data:`. It is the mesh event plus two environment coordinates, flattened:

```json
{
  "schema_version": "1",
  "id": "<environment_id>:<seq>",
  "environment_id": "<uuid>",
  "seq": 1303663,
  "kind": "session.state_changed",
  "time": "2026-10-20T23:37:13.123456789Z",
  "app": "tether",
  "session_id": "f8a403e8-a6e5-480c-b766-6225e5b76cb8",
  "subject": "urn:session:f8a403e8-a6e5-480c-b766-6225e5b76cb8",
  "process": {"scope": "session"},
  "source": {"channel": "tether.events", "confidence": 1},
  "actor": {"urn": "msg://service/tether/<environment_id>", "kind": "service"},
  "generation": 1,
  "source_sequence": 1303663,
  "cursor": "1303663",
  "content_type": "application/json",
  "visibility": "local",
  "truncated": false,
  "payload": { }
}
```

`payload` is the stored payload verbatim and depends on `kind`. Unknown kinds are delivered unchanged: a client ignores kinds it does not know. `actor` is the reporting daemon, not the user or tool behind the event. `generation` is the projection's generation (1), not a shim generation. `time` is the time the event was recorded.

Kinds a capacity view uses (SHIPPED unless marked):

| `kind` | `payload` |
|---|---|
| `session.state_changed` | `{"from","to","exit_code","detail"?}`: **lifecycle** only, see 5.2. The persisted state never says `idle` (see the known gap in 5.3) |
| `session.health_changed` | `{"from","to","reasons":[...],"since"}` where `to` is `healthy`/`degraded`/`unhealthy`/`unknown`: **runtime health, not work-state** |
| `session.request_status` | `{"turn_id","request_id","request_kind":"approval"\|"question","open","source_sequence","terminal"?}`: the durable record of an interactive request; the pending flags are reduced from these |
| `session.turn_routed` | routing record for a turn |
| `session.turn_output` | final turn result (**contains model text**) |
| `session.output` | raw provider output frames (**contains model text and tool arguments/results**) |
| `tool.call.started` / `tool.call.finished` | `{"server","tool","args_bytes","args_schema_fp"}` and `{"ok","error_class","duration_ms","result_bytes"}`: sizes and fingerprints only, no arguments or results |
| `session.work_state_changed` | **PROPOSED** (CW-20261021-0003), see 5.3 |
| `session.projection_changed` | **PROPOSED** (CW-20261021-0003), see 5.5 |

## 5. Snapshot and session projection

### 5.1 Environment snapshot (SHIPPED)

```json
{
  "environment_id": "<uuid>",
  "high_water_seq": 1308361,
  "earliest_available": 1,
  "sessions": [ { "...session projection (5.2)..." } ]
}
```

`sessions` holds every session row, ended ones included (a client filters on `session_state`). It is ordered by session id, which is not an activity order.

### 5.2 Session projection

Fields are grouped by what they mean. **Lifecycle, work-state and connectivity are three different things and are never merged into one field.**

| Field | Mark | Meaning |
|---|---|---|
| `id` | SHIPPED | session UUID, the join key |
| `logical_agent_id` | SHIPPED | the agent definition the session runs; many sessions share one |
| `kind` | SHIPPED | provider kind (`cli`, `api`, ...) |
| `route` | SHIPPED | opaque routing JSON; omitted if the stored value is not valid JSON |
| `session_state` | SHIPPED | **lifecycle**: `starting` \| `running` \| `detached` \| `paused` \| `orphaned` \| `ended` |
| `instance_status` | SHIPPED | `starting` \| `running` \| `waiting` \| `stopping` \| `stopped` \| `rejected` |
| `instance_detail` | SHIPPED | `{"waiting": "approval"\|"input"}` while waiting; `{"stopped": "completed"\|"failed"\|"canceled"}` once ended; `{}` otherwise |
| `last_activity` | SHIPPED | newest of the row's `updated_at` and its latest event time. Zero time (`0001-01-01...`) means unknown. **PROPOSED rename**: also published as `last_activity_at` (same value), `last_activity` stays for compatibility |
| `pending_question`, `pending_approval` | SHIPPED | a request of that kind is open |
| `pending_question_known`, `pending_approval_known`, `pending_known` | SHIPPED | `false` means "never observed": the pending flag is unknown, **not** "clear". `pending_known` is true only when both kinds have been observed, or the session ended |
| `instance_id` | NOT POPULATED | dropped, see section 1 |
| `provider_id`, `worker_id`, `project_id` | PROPOSED (CW-20261021-0004) | nullable strings; `null` = not recorded |
| `work_state` | PROPOSED (CW-20261021-0003) | see 5.3 |
| `connectivity` | PROPOSED (CW-20261021-0003) | see 5.4 |
| `freshness` | PROPOSED (CW-20261021-0003) | see 5.4 |
| `operations` | PROPOSED (CW-20261021-0004) | see section 6 |

Lifecycle rules (SHIPPED, from the reducer): `created`/`launching`/`starting` map to `starting`; `completed`/`failed`/`killed` map to `ended` with `instance_detail.stopped` `completed`/`failed`/`canceled`; an ended session has all pending flags `false` and all `*_known` flags `true`; an unrecognised stored state maps to `orphaned` (connectivity unknown cannot imply an outcome). A disconnected worker never turns a session into a successful terminal state by itself.

### 5.3 Work-state (PROPOSED, CW-20261021-0003)

`work_state` is what the runtime is doing now, separate from the lifecycle:

```json
"work_state": {"value": "idle", "since": "2026-10-21T00:12:01.5Z", "known": true}
```

`value` is one of `idle`, `processing`, `awaiting_approval`, `awaiting_input`, `unknown`. `awaiting_*` is derived from the open request flags, so it cannot disagree with `pending_*`. `unknown` (with `known: false`) is reported while no worker state has been observed since the session started or since the owning worker disconnected; it is never defaulted to `idle`.

**Known gap this fixes:** a worker's `idle` frame is normalized to the persisted lifecycle state `running` before `session.state_changed` is written (`worker_ingest.go`), so that event is always `running -> running` and carries no work-state. The fix emits a durable `session.work_state_changed` **before** the normalization, **only on an actual change** of value: `{"from","to","at"}` with the values above. It is retained like the other session state events. A snapshot reads the latest such event per session, so a client that reconnects reconstructs `work_state` from the snapshot alone, with no `/sessions/{id}/health` call. `/sessions/{id}/health` `live_state` stays the live source for health and is not part of this contract.

### 5.4 Connectivity and freshness (PROPOSED, CW-20261021-0003)

```json
"connectivity": {"value": "connected", "since": "2026-10-21T00:11:50Z"},
"freshness": {"observed_at": "2026-10-21T00:12:30Z", "last_event_at": "2026-10-21T00:12:29Z", "work_state_at": "2026-10-21T00:12:01Z"}
```

`connectivity.value`: `connected`, `disconnected` (the owning worker is not connected: the hub cannot tell "ended" from "out of reach"), `local` (hosted by the hub itself) or `unknown`. `freshness` is timestamps only; the server does **not** compute a "stale" boolean because the threshold is a presentation decision. `observed_at` is the server time the projection was built.

### 5.5 Session projection event (PROPOSED, CW-20261021-0003)

`session.projection_changed` carries the **whole** session projection (5.2), identical in shape to a snapshot item, produced by the **same function** as the snapshot. A client replaces its copy of that session; it never merges deltas and never runs a pending-request reducer. It is idempotent by `seq`.

It is emitted on any change of: lifecycle, work-state, pending flags, connectivity, provider/worker/project, or operations. A change of `last_activity` alone is coalesced with **trailing-edge** semantics: at most one event per 10 s per session, and the last value is always delivered (no lost final update; `last_activity_at` ends at its final value). Every other change is emitted immediately. Raw events (`session.state_changed`, `session.request_status`, ...) stay available and unchanged.

## 6. Operation capabilities (PROPOSED, CW-20261021-0004)

```json
"operations": {"send_turn": true, "send_input": false, "attach": true, "stop": true, "resize": false}
```

Each value is `true`, `false`, or `null` (not known). It states what the **runtime supports for this session**. It says nothing about whether the caller is allowed: authorization is the caller's own scopes (section 2) and is never embedded in the session item. `streaming_stdio: false` on the health endpoint must not be read as "read-only": JSON-RPC providers are interactive. The exact mapping is verified per provider (claude-code, codex-app-server, antigravity) in CW-20261021-0004 and published before it ships. Lifecycle control semantics (start/stop/end) are out of scope of this contract.

## 7. Payload-light view (PROPOSED, approved by the architect)

`GET /environment/events?view=projection` (and `/sessions/{id}/stream?view=projection`) delivers only a **server-defined allowlist** of kinds, and for each of them only an allowlisted set of payload paths. It is built on the mechanism the event pipeline already has (`internal/events/pipeline_registry.go`): kinds admitted as class `Metadata` carry a per-kind payload allowlist (`redact.Allowlist`), while `Compatibility` kinds are passed through whole. The view never delivers a `Compatibility` or unregistered kind in full.

| Kind in the view | Payload paths delivered |
|---|---|
| `session.projection_changed` (PROPOSED) | the whole session projection (5.2) |
| `session.work_state_changed` (PROPOSED) | `from`, `to`, `at` |
| `session.state_changed` | `from`, `to`, `exit_code` only (free-text `detail` is dropped) |
| `session.health_changed` | `from`, `to`, `reasons`, `since` |
| `session.stalled` | `reason`, `denied` |

Never delivered: `session.output`, `session.turn_output`, `session.request_status` (its content is already reduced into the projection), `tool.call.*` and `tool_call_*` (tool names, error classes and attribution; they are metadata-class today but nothing in the capacity view needs them), `message.*`, `wake.*`, `session.turn_routed` (channel and message ids), and any kind not listed. A new kind is excluded until it is added here deliberately (fail closed).

Cursor behaviour is identical to the raw stream (section 3): excluded events are consumed, `seq` stays global, `synchronized` advances the cursor, `gap` is unchanged. **This is a leak-prevention view, not access control.** Any `tether.read` holder can still ask for the raw feed. The Tachyon backend must therefore request `view=projection` server-side and must **not** hand the credential to the browser. A projection-only scope would be the real control; it is deferred (CW-20261020-0047, low priority). The view grants nothing the raw stream does not already require: the same `tether.read`, the same visibility rules (operator-class events stay operator-only), no new scope, the raw stream is unchanged. The existing `kind=`, `app=`, `scope.*` filters still apply on top (AND); they can only narrow the view further.

## 8. Populated and unpopulated, today

| Item | Today |
|---|---|
| `id`, `logical_agent_id`, `kind`, `route`, `session_state`, `instance_status`, `instance_detail`, `last_activity`, `pending_*` | populated |
| `pending_*_known` | populated; `false` until a request of that kind has been observed for the session |
| `instance_id` | never populated, dropped |
| `work_state`, `connectivity`, `freshness`, `provider_id`, `worker_id`, `project_id`, `operations`, `last_activity_at`, `session.projection_changed`, `session.work_state_changed`, `view=projection` | not yet; arrive with CW-20261021-0003/0004 |
