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

`POST /events` with a bearer credential. The `Tether-Protocol` header is **not** required for this route (it is the one write on the remote listener's header-exempt list, behind authentication); it is accepted if sent. The body is one event, or
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
| 401 | no verified credential (enforce) | retry with backoff: a credential may be rotated or not yet issued |
| 403 `scope_required` | credential lacks `tether.events` | retry with backoff: the scope may be granted later |
| 403 `app_mismatch` | an event names an app the credential is not bound to | terminal: drop, count and report; a retry can never succeed |
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
is a host credential and is not bound to one app (dev decision: it can publish as
any registered app, so the services on a host can relay through it; a per-device
app allowlist stored with the grant is the release posture, CW-20261020-0031).
The local operator and anonymous callers (observe/off) may also publish any app.
For every caller the kind must be owned by the event's own app, and the stored
actor is the credential (a device is `msg://device/...`, `verified: token`). The kinds an app may publish are the libs catalog
`producers.json`; a kind outside it is `forbidden_kind`, and a kind outside the
catalog is `unknown_kind`.

While `event_stage_mode` is `shadow`, kinds that are not in the catalog are
accepted; with `enforce` they are rejected.

## What the hub does to a payload

The producer sanitizes before sending. The hub scrubs again with the shared
redaction engine (`security/redact`) and records the count in `class.redactions`:

- A kind whose catalog payload schema is a plain tree of declared properties gets
  an **allowlist** derived from that schema: only the declared paths are kept and
  anything else is removed, so a field the schema does not declare never reaches
  the log. To carry a new field, add it to the kind's payload schema in the libs
  catalog.
- A kind with no schema, a free-form object (`data.emitted`), or a schema using
  composition (`oneOf`, `$ref`, ...) cannot be reduced to a path list. It gets the
  **denylist**: secrets and PII are scrubbed and the shape is kept. This
  fallback is a DEV policy, to be revisited at release posture; secret and PII
  scrubbing always applies.
- A schema that is declared but cannot be read, parsed or compiled into a policy
  is **not** a reason to fall back. The daemon builds every catalog kind's policy
  at start and refuses to enable ingest, with an error naming the kind and the
  schema. If a kind's policy is still unavailable when an event arrives, the
  payload is not stored (`{"sanitize_error":"policy_unavailable"}`,
  `class.sanitized=false`) and the daemon logs the cause.
- A payload that cannot be sanitized (oversized, opaque) is replaced with
  `{"sanitize_error":"<class>"}` and the event is stored with
  `class.sanitized=false`.
- The log never stores a secret-class event: a payload the producer calls
  `secret` is withheld.

## Retries and ordering

The event `id` is the only dedupe key, and ids are one global namespace: the
key is the id alone, never (producer, id). An event with an id that is already
stored but a different app is a different event claiming the id, so it is an
`id_conflict`, whoever sends it. The hub commits the event and a digest of
it in one transaction, so an event never exists without its digest, and the
event is acknowledged and fanned out only after that commit. A failed or
interrupted write stores nothing and is not acknowledged; the producer retries.

The digest is the SHA-256 of the whole producer-supplied envelope in canonical
form (RFC 8785, JCS). A retry that resends the same event is a `duplicate`
carrying the original cursor. The same id with any producer-controlled
difference (kind, app, subject, scope including `project_id`, correlation,
class as sent, `time`, process, source, payload, and the rest) is a different
event claiming the id: `rejected` with `id_conflict`.

Excluded from the digest, because the hub or the transport owns them and they
may differ between two sends of one event: `recorded_at`, `cursor`, `actor`.
Because `actor` is not in the digest, a byte-identical resend from another
principal under the same app is a `duplicate` (no new event; the original cursor
is returned). Service tokens are bound to one app, so this only concerns the
unbound credentials (the local operator and paired devices).
Key order and whitespace do not matter. The `mesh/eventbus` client resends the
stored bytes of an item unchanged (retries, spool replay, repeated `Deliver`),
so its retries are always duplicates.

An id that is stored but has no digest on record (written before the ingest
existed, or partially) is never acknowledged as a duplicate: it is `rejected`
with `id_conflict` ("stored without verifiable evidence") and the hub logs it.
Ordering across producers is the log cursor, never `sequence`.

## Client configuration (apps)

The libs client `mesh/eventbus` reads its settings from the environment, so every
app configures publishing the same way:

| Variable | Meaning |
|---|---|
| `HOLLIS_EVENTS_URL` | Base URL of the hub (the client appends `/events`; a URL already ending in `/events` is left alone). Unset means publishing is off and the client is a no-op. |
| `HOLLIS_EVENTS_SPOOL_DIR` | Optional directory for the bounded on-disk spool, so events survive a hub outage or a restart. |
| `HOLLIS_AUTH_TOKEN_FILE_<APP>` | File holding the app's bearer credential (a service credential bound to `<APP>`). |

The event's `app` must equal the app the credential is bound to; a mismatch makes
the whole request a 403 `app_mismatch` and nothing is written.
