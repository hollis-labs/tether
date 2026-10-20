# Publishing events to the hub (`POST /events`)

Any app can publish events to Tether's event log and any app can read them back,
without importing Tether. The wire format is the runtimeevents envelope v1.1
(libs `harness/core/adapters/runtimeevents`). The request, the answer, the
limits and the reject codes are defined once, in the libs `ingest` package
(`runtimeevents/ingest`), with JSON Schemas and fixtures for other languages.
This page is the hub's side of that contract.

The route is off until the operator turns it on:

```yaml
# global.yaml
daemon:
  event_ingest:
    enabled: true
    rate_per_second: 200   # per publishing principal; optional
    burst: 1000            # optional
```

## Request

`POST /events` with a bearer credential. The body is one event, or
`{"events":[...]}` with 1 to 100 events. At most 1 MiB per request and 64 KiB per
payload.

The producer sets `schema_version` (`"1"`), `id` (`evt_` + 32 hex, minted once and
reused on every retry), `kind`, `time`, `app`, `source.channel`, `sequence`
(per producer, per `source.channel`, per `generation`) and one of `session_id` or
`subject` (`msg://<noun>/<app>/<id>`). The rest is optional.

The hub assigns `cursor` and `recorded_at`, and overwrites `actor`, the sanitize
fields of `class`, `class.visibility` and the scope dimensions it owns
(`environment_id`, `authority`, `worker_id`, `host`). A producer never sends
`visibility: local`. A producer may ask for `operator`; it cannot widen to `mesh`.
Producer scope dimensions (`task_id`, `project_id`, ...) are kept.

## Answer

A request that was understood is answered `200` with one result per event, in
order:

```json
{"results":[
  {"id":"evt_...01","status":"accepted","cursor":"4812"},
  {"id":"evt_...02","status":"duplicate","cursor":"4790"},
  {"id":"evt_...03","status":"rejected","code":"forbidden_kind","message":"..."}
 ],"accepted":1,"duplicate":1,"rejected":1}
```

- `accepted`: stored; `cursor` is the log position (the same value a subscriber
  resumes from).
- `duplicate`: the id was already stored for the same event. This is success; it
  carries the original cursor and nothing is published a second time.
- `rejected`: not stored; do not retry it unchanged. Codes: `unknown_kind`,
  `schema_invalid`, `forbidden_kind`, `payload_too_large`, `id_conflict` (the id is
  stored for a different event).

One rejected event never fails its siblings. Whole-request failures use the HTTP
status:

| Status | Meaning | Producer |
|---|---|---|
| 400 | body is not an event or `{"events":[...]}`, or the batch is empty | fix, do not retry |
| 401 | no verified credential (enforce) | fix |
| 403 | credential lacks `tether.events` (`scope_required`), or an event names an app the credential is not bound to (`app_mismatch`) | fix |
| 413 | more than 100 events or 1 MiB | split |
| 429 | rate limited; `Retry-After` seconds | back off |
| 503 | the log could not be written; `Retry-After` | retry the same request (ids make it safe) |

## Who may publish

The publish check follows the local API's identity mode (`off`, `observe`,
`enforce`; `HOLLIS_AUTH_TETHER` can only tighten it):

- `enforce`: a verified credential holding `tether.events` is required. Agent
  session tokens never publish. `tether.operate` and `tether.worker` contain
  `tether.events`; the local operator holds everything.
- `observe`: the request is allowed and a would-deny is logged.
- `off`: allowed, and the actor is recorded as unverified.

A service credential (`msg://service/<app>/<env>`) is bound to its `<app>`: an
event with another `app` makes the whole request a 403. A paired worker device
publishes as app `tether`. The local operator and anonymous callers (observe/off)
may publish any app. The kinds an app may publish are the libs catalog
`producers.json`; a kind outside it is `forbidden_kind`, and a kind outside the
catalog is `unknown_kind`.

While `event_stage_mode` is `shadow`, kinds that are not in the catalog are
accepted; with `enforce` they are rejected.

## What the hub does to a payload

The producer sanitizes before sending. The hub scrubs again with the shared
redaction engine (`security/redact`, denylist mode, private) and records the
count in `class.redactions`. A payload that cannot be sanitized (oversized,
opaque) is replaced with `{"sanitize_error":"<class>"}` and the event is stored
with `class.sanitized=false`. The log never stores a secret-class event: a
payload the producer calls `secret` is withheld.

Per-kind payload-path allowlists replace the denylist as kinds are registered
(CW-20261020-0012).

## Retries and ordering

The event `id` is the only dedupe key. The hub remembers a digest of the event as
sent (kind, app, subject, session, channel, sequence, generation, payload with
sorted keys) beside the id, so a retry is a `duplicate` and a different event
reusing the id is an `id_conflict`. Ordering across producers is the log cursor,
never `sequence`.
