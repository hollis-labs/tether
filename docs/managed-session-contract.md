# Managed-session snapshot and event contract

Status: **v3** (Archie and Portfolio Architect review requests of 2026-10-21 folded in) for CW-20261021-0001 (managed-agent-capacity workstream).
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
  - `event: synchronized`, `data: {"environment_id","seq"}`: sent after every drain, at the cursor the server has reached. **A client that sees it may advance its stored resume cursor to `seq`** even if no data event was delivered (this is what makes a filtered stream resumable). In the payload-light view (section 7) its data also carries `"view":"projection"`. It is sent after **every drain**, whether or not the cursor moved, so an idle stream carries about one per `PollInterval` (default 1 s). **There are no SSE comment keepalives.** A client detects a dead stream by the connection closing or erroring (an `EventSource` error is the expected signal) or by the absence of any frame for several `PollInterval`s, and then reconnects with its last `seq`; the interval is the server's setting and is not announced in the stream.
  - `event: gap`, `id: <after_seq>`, `data: {"environment_id","reason","earliest_available","high_water_seq","snapshot_required","retry_after_seconds"?}`: the server cannot continue from the cursor and **closes the stream**. The cursor is not advanced. `snapshot_required: true` means take a new snapshot and resume from its `high_water_seq`.
- `gap` reasons: `ahead` (cursor is beyond the head), `purged` (history before `earliest_available`, or an interior hole, was retained away), `too_large` (more than the catch-up window, 2048 events, is missing), `restart` (the server could not map an event or its live feed ended), `event_id_invariant` (a stored row has no usable event id), `backfill_in_progress` (not a snapshot case: `snapshot_required: false`, retry after `retry_after_seconds`).
- Duplicate delivery is possible across reconnects. **Clients compare by `seq` only**: apply an event iff its `seq` is greater than the last applied `seq` of the environment, and a snapshot only if its `high_water_seq` is greater. Never compare `observed_at` or any other time inside a projection: a projection event is built **at emission** (when the change happened) and a snapshot item is built **at read**, so the two differ by construction in time fields such as `freshness.observed_at`.
- The server also re-reads the durable log on a timer (default 1 s) as a safety net next to the live wake-ups, and sends a `synchronized` frame after every drain, so an idle stream carries about one `synchronized` frame per second. Treat it as a keepalive and a cursor advance, not as a signal to poll. Its disposition (documented safety net or change-driven) is CW-20261021-0005; clients add **no polling** of their own.

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
  "trace": {},
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
| `session.state_changed` | `{"from","to","exit_code"?,"detail"?,"reason"?}` (`reason` is an internal recovery code such as `process_missing`; `detail` is free text from a worker): **lifecycle** only, see 5.2. The persisted state never says `idle` (see the known gap in 5.3) |
| `session.health_changed` | `{"from","to","reasons":[...],"since"}` where `to` is `healthy`/`degraded`/`unhealthy`/`unknown`: **runtime health, not work-state** |
| `session.request_status` | `{"turn_id","request_id","request_kind":"approval"\|"question","open","source_sequence","terminal"?}`: the durable record of an interactive request; the pending flags are reduced from these |
| `session.turn_routed` | routing record for a turn |
| `session.turn_output` | final turn result (**contains model text**) |
| `session.output` | raw provider output frames (**contains model text and tool arguments/results**) |
| `tool.call.started` / `tool.call.finished` | `{"server","tool","args_bytes","args_schema_fp"}` and `{"ok","error_class","duration_ms","result_bytes"}`: sizes and fingerprints only, no arguments or results |
| `session.work_state_changed` | **PROPOSED** (CW-20261021-0003), see 5.3 |
| `session.projection_changed` | **PROPOSED** (CW-20261021-0003), see 5.5 |

## 5. Snapshot and session projection

### 5.1 Environment snapshot (SHIPPED, but slow and failing under load on the deployed hub until CW-20261021-0002 is verified live)

```json
{
  "environment_id": "<uuid>",
  "high_water_seq": 1308361,
  "earliest_available": 1,
  "sessions": [ { "...session projection (5.2)..." } ]
}
```

**Do not treat `GET /environment/snapshot` as available** until CW-20261021-0002 is deployed and verified (section 8): on the deployed hub it took 10.1 s on a cold call and has returned `500`. The per-session snapshot `GET /sessions/{id}/snapshot` has answered `200`. `sessions` holds every session row, ended ones included (a client filters on `session_state`). It is ordered by session id, which is not an activity order.

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

Meaning of the non-terminal values: **`detached`** describes the daemon's connection to a still-live runtime, **not** a client detach: it is set when the daemon restarts or a recovery start fails while the child is not proven gone, and the shim reconciler later reattaches it (the retained workspace and credentials are kept). **`orphaned`** means the process identity vanished (reaper `process_missing`, reconcile disabled) or the stored state is unrecognised: its credentials and bindings are revoked, it is non-terminal, and it is a **diagnostic value, not an outcome**. **`paused`** is part of the mesh vocabulary but Tether has no paused state: it is **never emitted today**. `starting` covers the stored states `created`, `launching` and `starting`. (`ready` is defined but never stored on a session; if it ever were, the reducer would show it as `orphaned`, the unrecognised-state value.) **Clients must ignore unknown values** of `session_state`, `instance_status`, `instance_detail.*`, `work_state.value` and `connectivity.value`: the vocabularies grow additively, and an unknown value is rendered as unknown, never as an error or as idle.

### 5.3 Work-state (PROPOSED, CW-20261021-0003)

`work_state` is what the runtime is doing now, separate from the lifecycle:

```json
"work_state": {"value": "idle", "since": "2026-10-21T00:12:01.5Z", "known": true}
```

`value` is one of `idle`, `processing`, `awaiting_approval`, `awaiting_input`, `unknown`. `awaiting_*` is derived from the open request flags, so it cannot disagree with `pending_*`. `unknown` (with `known: false`) is reported while no work-state has been observed; it is never defaulted to `idle`. Once the lifecycle has ended, `work_state` is **absent** from the item. When the owning worker disconnects (connectivity `disconnected`, 5.4) `value` becomes `unknown` with `known: false`, and the previous reading is kept as `last_known`: `{"value": "idle", "at": "..."}`, so a UI can say "was idle at T, now unobservable" without inventing a current value. `last_known` is absent while the value is known.

**Known gap this fixes:** a worker's `idle` frame is normalized to the persisted lifecycle state `running` before `session.state_changed` is written (`worker_ingest.go`), so that event is always `running -> running` and carries no work-state. The fix emits a durable `session.work_state_changed` **before** the normalization, **only on an actual change** of value: `{"from","to","at"}` with the values above. It is retained like the other session state events. **Durability:** the current work-state and its `since` are kept on a state row written in the **same transaction** as the event (the pattern `environment_request_status` already uses), and the snapshot reads that row, not the log, so event retention can never turn a healthy long-idle session into `unknown`. This needs a small additive table (a migration); the architect's confirmation of that migration is pending, and until it is deployed the contract's `work_state` is not on the hub at all. If the migration were refused the fallback would be reading the latest event, with the documented caveat that a session idle past event retention reports `unknown` until its next transition. A client that reconnects reconstructs `work_state` from the snapshot alone, with no `/sessions/{id}/health` call. `/sessions/{id}/health` `live_state` stays the live source for health and is not part of this contract; once the work-state is deployed it is a second source for the same fact and is retired from capacity use, so there is one authoritative publisher.

### 5.4 Connectivity and freshness (PROPOSED, CW-20261021-0003)

```json
"connectivity": {"value": "connected", "since": "2026-10-21T00:11:50Z"},
"freshness": {"observed_at": "2026-10-21T00:12:30Z", "last_event_at": "2026-10-21T00:12:29Z", "work_state_at": "2026-10-21T00:12:01Z"}
```

`connectivity.value`: `connected`, `disconnected` (the owning worker is not connected: the hub cannot tell "ended" from "out of reach"), `local` (hosted by the hub itself) or `unknown`. `freshness` is timestamps only; the server does **not** compute a "stale" boolean because the threshold is a presentation decision. `observed_at` is the server time the projection was built.

### 5.5 Session projection event (PROPOSED, CW-20261021-0003)

`session.projection_changed` carries the **whole** session projection (5.2), identical in shape to a snapshot item, produced by the **same function** as the snapshot. A client replaces its copy of that session; it never merges deltas and never runs a pending-request reducer. It is idempotent by `seq`.

Its payload is the snapshot item **without `route`** (see 7.1), built **at emission**; `freshness.observed_at` is the emission time. **Change triggers, exhaustive:** lifecycle (`session_state`, `instance_status`, `instance_detail`), `work_state.value`, the pending flags, `connectivity.value`, `provider_id` / `worker_id` / `project_id`, and `operations`: each publishes immediately. **`freshness.*` never triggers an event.** `last_activity_at` alone is the only coalesced trigger (see the wording above). **`last_activity_at` is not a liveness signal**: a quiet session produces no event, so the absence of events means nothing; liveness is `connectivity` plus `work_state`. The payload fields are an explicit allowlist, so a field added to the session item later does not appear in this event until it is added to the allowlist deliberately (fail closed on fields, not only on kinds).

Raw events (`session.state_changed`, `session.request_status`, ...) stay available and unchanged.

## 6. Operation capabilities (PROPOSED, CW-20261021-0004)

```json
"operations": {"send_turn": true, "send_input": false, "attach": true, "stop": true, "resize": false}
```

Each value is `true`, `false`, or `null` (not known). It states what the **runtime supports for this session**. It says nothing about whether the caller is allowed: authorization is the caller's own scopes (section 2) and is never embedded in the session item. `streaming_stdio: false` on the health endpoint must not be read as "read-only": JSON-RPC providers are interactive. The exact mapping is verified per provider (claude-code, codex-app-server, antigravity) in CW-20261021-0004 and published before it ships. **UI rule for `null`:** a `null` operation means not known or not verified for this provider; the UI shows it as unknown/unverified and must neither hide the control nor enable it by default. A value becomes `true` or `false` only for a provider whose capability set has been verified in CW-20261021-0004, and only then. Lifecycle control semantics (start/stop/end) are out of scope of this contract.

## 7. Payload-light view (PROPOSED, approved by the architect)

`GET /environment/events?view=projection` (and `/sessions/{id}/stream?view=projection`) delivers only a **server-defined allowlist** of kinds, and for each of them only an allowlisted set of payload paths. It is built on the mechanism the event pipeline already has (`internal/events/pipeline_registry.go`): kinds admitted as class `Metadata` carry a per-kind payload allowlist (`redact.Allowlist`), while `Compatibility` kinds are passed through whole. The view never delivers a `Compatibility` or unregistered kind in full.

| Kind in the view | Payload paths delivered |
|---|---|
| `session.projection_changed` (PROPOSED) | the session projection (5.2) **without `route`** (5.5); an explicit allowlist of fields |
| `session.work_state_changed` (PROPOSED) | `from`, `to`, `at` |
| `session.state_changed` | `from`, `to`, `exit_code` only (free-text `detail` and the `reason` code are dropped) |
| `session.health_changed` | `from`, `to`, `reasons`, `since` |
| `session.stalled` | `reason`, `denied` |

Never delivered: `session.output`, `session.turn_output`, `session.request_status` (its content is already reduced into the projection), `tool.call.*` and `tool_call_*` (tool names, error classes and attribution; they are metadata-class today but nothing in the capacity view needs them), `message.*`, `wake.*`, `session.turn_routed` (channel and message ids), and any kind not listed. A new kind is excluded until it is added here deliberately (fail closed).

**Negotiation: a client must see positive evidence that the view was honored.** A server that predates this view ignores the unknown `view` query parameter and silently returns the raw feed, so the request alone proves nothing. A server that honors `view=projection` therefore answers with the response header

```
Tether-View: projection; v=1
```

on `/environment/events` and `/sessions/{id}/stream`, and repeats it in every `synchronized` frame as `"view":"projection"` (the raw stream's frames are unchanged and carry neither). The header is **absent on raw responses**. A client that asked for the view and does not receive the header **MUST fail closed**: it must not forward the stream to a browser. An unknown `view` value, or a repeated `view`, is `400 invalid view` (never ignored). `v` is the view's version; a later incompatible allowlist is `v=2`.

Cursor behaviour is identical to the raw stream (section 3): excluded events are consumed, `seq` stays global, `synchronized` advances the cursor, `gap` is unchanged. **This is a leak-prevention view, not access control.** Any `tether.read` holder can still ask for the raw feed. The Tachyon backend must therefore request `view=projection` server-side and must **not** hand the credential to the browser. A projection-only scope would be the real control; it is deferred (CW-20261020-0047, low priority). The view grants nothing the raw stream does not already require: the same `tether.read`, the same visibility rules (operator-class events stay operator-only), no new scope, the raw stream is unchanged. The existing `kind=`, `app=`, `scope.*` filters still apply on top (AND); they can only narrow the view further. **Fields are allowlisted, not only kinds:** each kind's payload goes through the registered per-kind path allowlist (the `Metadata` admission class), and the projection event's fields are an explicit list (5.5), so nothing is delivered by being new. **Limitation, stated plainly:** today a `tether.read` credential **cannot** be limited server-side to this view; a holder may request the raw stream. The control is the Tachyon backend requesting the view server-side with a credential it never gives to a browser; a projection-only scope is deferred (CW-20261020-0047).

### 7.1 Privacy audit of the projection (CW-20261021-0001)

Every field of the session projection was checked for what it can carry, in the code and in the production data (581 sessions). Fields that go to a browser through `view=projection` are only those below.

| Field | What it can contain | In the projection event / view |
|---|---|---|
| `id` | session UUID | yes (join key) |
| `route` | **Stored data, not only fixtures:** `route_json` has exactly one writer (session creation, through `ResolveRoute`), so a stored route is always `{"channel": <name>, "kinds": [...]}`. `channel` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` (at most 64 ASCII characters, one URL path component: no `/`, `:`, `@`, spaces or other separators, so no URL, path, address or credential can be stored; a dotted label such as `owner-inbox` or even a host-like name is expressible, it is an operator-chosen label). `kinds` is a subset of `final`, `question`, `approval`, `failure`. In production 73 of 581 sessions have a route and none has any other key. | **omitted**: the capacity view does not need it, and the event never carries it. The snapshot item keeps it (SHIPPED) and its schema now allows only these two keys; the Tachyon backend must not forward it to the browser |
| `logical_agent_id` | a catalog agent id (`general`, `portfolio-manager`, ...), an identifier the operator chose; not a secret | yes (needed to label capacity) |
| `kind` | provider kind (`cli`, `api`); may be the empty string for sessions created without one | yes |
| `session_state`, `instance_status` | closed enums | yes |
| `instance_detail` | closed enums only (`waiting`: approval/input; `stopped`: completed/failed/canceled); no free text | yes |
| `last_activity`, `pending_*` | a time and booleans | yes |
| `work_state` | closed enum and a time | yes |
| `provider_id`, `worker_id`, `project_id` (PROPOSED) | catalog/provider id, worker id and project id strings; identifiers, not secrets; **no workspace path, repo root, host or URL is ever added** to the projection | yes |
| `session.state_changed` payload in the view | `from`, `to`, `exit_code` only; the free-text `detail` is dropped | reduced |
| `session.health_changed` payload | `reasons` are closed codes (`session_running`, `repeated_tool_denials`, `provider_session_lost`, `runtime_failed`, `runtime_orphaned`, ...), never text | yes |
| `session.stalled` payload | `reason` is a closed code, `denied` a count | yes |

Rule: a field with free text, a path, a host, an address or a credential is never added to the projection. Adding any field to it is a contract change that repeats this audit.

## 8. Known issues of the SHIPPED endpoints

- **`GET /environment/snapshot` can be slow and can fail under write load until CW-20261021-0002 is deployed.** Measured on the production hub: 10.1 s on the first call, 1.9 s warm (216 KB, 581 sessions), because the per-session last-event lookup sorted every event of every session. The fix (an index lookup, about 4x faster on a copy of the production data) and a classified answer (a transient failure becomes `503` with `Retry-After`, not an opaque `500`) are in review. Until it is deployed a client treats `500`/`503` as retryable with backoff. The cause of the observed 500s is not proven.
- **The 1 s durable-log sweep** (section 3) is a documented safety net today; whether it is replaced by change-driven delivery is CW-20261021-0005.
- **`work_state`, connectivity and capabilities are not on the hub yet** (section 10).

## 9. Schemas and fixtures

`docs/fixtures/managed-session` (added with the CW-20261021-0003 work-state slice) holds JSON Schema 2020-12 for the session (`schema/session.schema.json`), the snapshot and the event envelope, and fixtures: the snapshot with one session per lifecycle / work-state / pending variant, a single session, one event per kind, and the exact SSE frames (`synchronized`, `gap`: purged, too_large, ahead, restart). Everything marked SHIPPED is **generated from the real types and handler by golden tests** (`UPDATE_FIXTURES=1` regenerates), so the examples cannot drift from the code. The PROPOSED additions are schema-valid examples under `proposed/` until their tasks ship. Read `README.md` there for which files are code-backed.

## 10. Populated and unpopulated, today

| Item | Today |
|---|---|
| `id`, `logical_agent_id`, `kind`, `route`, `session_state`, `instance_status`, `instance_detail`, `last_activity`, `pending_*` | populated |
| `pending_*_known` | populated; `false` until a request of that kind has been observed for the session |
| `instance_id` | never populated, dropped |
| `work_state`, `session.work_state_changed` | arrive with the CW-20261021-0003 work-state slice (event, snapshot field and reducer); not available until it is deployed and announced |
| `connectivity`, `freshness`, `session.projection_changed`, `view=projection` and the `Tether-View` header | not yet; later CW-20261021-0003 slices |
| `provider_id`, `worker_id`, `project_id`, `operations`, `last_activity_at` | not yet; CW-20261021-0004 |
