# Environment streams (protocol 1)

The Unix-socket API adds four read surfaces: `GET /environment/snapshot`,
`GET /environment/events?after_seq=N`, `GET /sessions/{id}/snapshot`, and
`GET /sessions/{id}/stream?after_seq=N`. Supply `Tether-Protocol: 1`.
These endpoints do not start a remote listener or grant remote authority.

Take a snapshot first. Its `high_water_seq` and session projection come from
one SQLite read transaction. Subscribe with that cursor, apply each event once
by `(environment_id, seq)`, and retain the latest `synchronized` marker's cursor.
The server registers live notifications before reading its durable catch-up;
notifications only trigger another bounded durable read. Session views retain
the environment's cursor, including synchronized advances past unrelated
sessions. Lifecycle row updates and their existing subsequent event publication
are separate operations; state assignments and request reduction are idempotent.

Every SSE event uses `id: <seq>`, `event: <kind>`, and the complete mesh envelope
as its JSON data. `environment_id` and `seq` extend `mesh.Event` in the same
object. Published mesh clients can ignore these two additive fields. Payload
members with those names remain nested payload members and cannot shadow the
envelope coordinates. Original kinds and JSON payloads are retained, including
unknown kinds. Invalid persisted JSON ends the stream with a resnapshot signal.

The daemon uses the persisted producer envelope after the frozen-ID readiness
step described below. IDs are opaque strings; log cursors and producer counters
remain separate.

| Durable source | Mesh projection |
| --- | --- |
| Event-table id | `seq`, decimal `Cursor`, SSE `id:` |
| Persisted `event_id` | Envelope `ID`, unchanged |
| Producer sequence/generation/origin time | Preserved producer coordinates; log sequence is not substituted for a known producer sequence |
| Persisted scope/corr/actor/class/process | Corresponding v1.1 blocks; unknown fields remain absent |
| Persisted kind/payload | Unchanged `Kind` and approved payload (`null` if absent) |
| Missing producer coordinates | Durable `tether.events` source and log sequence |
| Missing author | Reporting hub service with `actor_verified: none`; no invented operator or delegation |

The legacy `scope` string remains a storage/filter field, separate from the
v1.1 scope block. Unknown runtime custody generation is not inferred from a
session name; the library's zero-generation compatibility value is one.
The default `environmentstream.New` constructor retains the old projection for
compatibility embedders. Daemon startup explicitly selects `NewStored` and never
falls back to the old mapping while backfill is pending.
Future durable kinds use the same lossless kind/payload mapping.

There are three independent cursor domains. The environment event cursor is
the `events` table's allocated sequence and survives full event retention. A
channel publication cursor is per channel. Raw attach's legacy `since_seq`
parameter names a zero-based **byte offset**, not either event sequence.

`gap` frames carry `reason`, `earliest_available`, `high_water_seq`, and
`snapshot_required: true`; they close the stream without advancing its last
accepted cursor to the head. Reasons are `purged` (including an interior hole),
`ahead`, `too_large` (more than the bounded catch-up allowance), and `restart`
(lost live subscription or unavailable durable read). Take a fresh snapshot
before resubscribing. A normal daemon restart with intact history can replay
from the old cursor; it does not imply the history was lost. Slow writes are
bounded by a deadline, and excessive backlog is an explicit gap, never a silent
skip. Clients must reconnect after transport EOF; this is not a global
exactly-once delivery promise.

The server projects connectivity and work separately. `detached` and `orphaned`
keep nonterminal work. Interactive request status is persisted atomically with
its event, keyed by exact session/turn/request identity; the current projection
survives event retention. Concurrent requests are independent. A matching
resolution, or an actual terminal event for that exact turn, closes a request;
an old source sequence cannot reopen it. The post-policy runtime hook stores
only identity/type/open status and source sequence, never prompt or answer
contents. Pending flags are positive observations. Their per-axis `*_known`
fields remain false for unobserved/unsupported producers; absent events do not
prove a clear interactive state. Only an actual ended session establishes both
flags as clear. Snapshots never read a request update beyond their high-water.

Raw attach uses the published `harness/v0.5.0` snapshot callback. Before the
first replay/live byte, `X-Tether-Attach-Oldest-Offset` and
`X-Tether-Attach-Next-Offset` report the atomic admission window
`[totalWritten-len(ring), totalWritten)`. `X-Tether-Attach-Cursor: byte-offset`
names the domain; `X-Tether-Attach-Evicted: true` means the requested byte offset
precedes retention. An empty broker legitimately reports `0/0`. Missing or
disabled sessions have no snapshot and therefore no invented bounds. These
are admission bounds, not continuously current ring positions or a guarantee
against subsequent raw live subscriber drops. Legacy injected adapters without
the callback retain their old stream behavior without fabricated metadata.

### Durable producer metadata

Migration 0086 completes the stored producer envelope: schema version, app,
subject, turn and parent IDs, raw offset, process identity, source channel and
confidence, producer sequence/generation, traceparent, reply correlation, actor
role and delegation chain. These nullable columns have no indexes and require no
backfill. Existing rows keep unknown metadata empty. Producer counters use decimal
TEXT so the full unsigned 64-bit range survives SQLite storage; they are not
stream cursors. A raw offset of zero remains distinct from an absent offset.

The origin timestamp is preserved in `at`; `recorded_at` is stamped by the log.
For origin times more than five minutes ahead of the log clock, `at` uses log time
so indexed retention cannot be evaded, and `origin_at` preserves the actual
producer timestamp. Event readers and live delivery return that true origin time;
the clamp only affects the stored retention key. Producer-supplied origin metadata
cannot override this value.
`events.id` remains the ordering and replay cursor, including when events arrive
with older origin timestamps. Replay, outbox, environment and session-history
reads hydrate the same stored envelope, including a retention tombstone's
`payload_digest`. Retried producer IDs return the original stored event.

This storage change does not switch environment-stream projection or enable raw
runtime ingress. Those follow-up changes consume the preserved metadata. Rollback
uses the previous binary; leave additive columns in place (migrations are forward
only).

## Derived native-event provenance

Native runtime payloads remain in process. The existing approved request-status
and turn-output builders persist their own payloads with native origin time,
process/source coordinates, scope, actor and correlation. The native event ID
becomes `causation_id`; `parent_id` retains its structural meaning. Derived rows
get fresh event IDs. Original payload-schema and sanitation claims are not
inherited by the changed payload. Sensitivity and PII floors accumulate across
a turn's contributing output, and its metadata survives retry replay.

An explicit native `sensitivity: secret` is a policy floor. Before any retry
journal or reply staging, Tether replaces the output text with a deterministic
`reply withheld: secret` notice naming only the session, turn and original byte
size. The routed requester receives that notice through the existing route;
the `session.turn_output` event contains metadata (`reason: secret_withheld`,
kind and size), without text or `payload_digest`. Session-local/native logs keep
their existing behavior. `/health.hardening.turn_output.secret_withheld` counts
new withholding in the current process.

The withheld output ID hashes only the domain `withheld/v1` and the session,
turn, kind and provider-result identity. The reducer produces one output per
completed turn; no body, body hash or size enters this ID. Non-withheld output
keeps its existing identity contract. Receipt verification checks the withheld
identity and exact notice metadata/body instead of a secret-content fingerprint.
A second producer claiming the same withheld identity fails and increments
`/health.hardening.turn_output.withheld_identity_violation`; a journal replay
continues the original output without making a new claim.

Before retry replay, an upgrade pass scrubs explicitly marked-secret records,
including expired files, then retires their old content-hash filenames. It
preserves the original retention timestamp. A crash between atomic replacement
of the body and retirement of the old filename leaves a safe notice, whose
rekeying resumes before it can enter a retry worker. Unmarked historical records
cannot be classified without detectors and keep their existing behavior.
Detector-based redaction and raw-kind admission remain gated on the published,
reviewed sanitizer and per-kind admission decisions.

The native-secret output gate currently applies to callback-fed runtimeevents
(the ACP ingress). Hosted Codex delivery and worker result frames do not carry a
native envelope class, so this slice cannot infer a secret floor for them and
does not claim to sanitize or withhold their unclassified output. The receipt
and accounting support is defensive preparation for future classified ingress.

The withholding notice's size is deliberately permitted metadata in
`secret_turn_output_routing_2026_10_16`; the identity contains neither size nor
content. Journal cleanup is defence in depth for explicitly marked records:
main's older records had no Origin field, so unmarked historical bodies cannot
be classified by this gate. For an old unrouted excerpt, size describes only
the retained excerpt; it cannot reconstruct the full body size or authenticate
a later full-body receipt. Duplicate-producer detection lasts while its journal
or published event evidence is retained; it is not a permanent identity ledger
after both have been purged.

Untagged native inputs cannot safely name one reducer turn. Their sensitivity
and PII floors apply to every open origin and the next turn; an untagged
TurnStarted creates no origin identity. Tracking is bounded to 64 origins.
Eviction carries classification floors forward, so ambiguity can withhold more
output but never silently downgrade it.

### Frozen legacy-ID storage

Migration 0087 creates only the singleton `event_identity` metadata table. It
creates no indexes, seeds no identity and backfills no events. The daemon-specific
constructor now selects the coordinated startup and projection source path;
ordinary inspection/compatibility constructors do not seed.

At that cutover, startup reads the environment UUID file and calls
`SeedEventIdentity` before starting event producers. The first call freezes the
UUID and the current maximum event sequence in one transaction; subsequent calls
must supply the same UUID and cannot move the cutover. A mismatch refuses startup.
The initial remaining-row count performs one scan; health reads thereafter use
the stored counters.

`BackfillEventIDs` changes at most 1,000 eligible rows per call, newest first. It
fills only NULL IDs at or below the frozen cutover with the literal
`<environment UUID>:<sequence>`. It never changes existing IDs, including IDs
minted before cutover. The cursor and progress commit with each chunk; failures
roll back the whole chunk and a restart resumes from the saved cursor. The
daemon background worker yields for 10 ms between chunks, observes
cancellation, and is joined before its database closes. It stops after a failed
chunk and records only a failure count; a fresh daemon resumes the committed
cursor. The backfill never drops an event. Migration 0093 installs an additive
insert guard: once seeded, NULL/empty new IDs are refused at insertion too.
The migration itself performs no backfill or history update.

Release coordination: startup seeding, complete ID minting, stored-ID wire
projection and retryable handling of unfinished legacy rows must ship together.
Approximately 281 already-minted rows in the observed hub database will change
visible environment-stream ID once, from the previous computed `<env>:<seq>` to
their already-stored `evt_` ID. Their stored IDs remain unchanged; consumers may
observe those events again. All wire IDs must remain JSON strings.

The activation rollout has two releases. Release N runs complete ID minting with
no seed or Map switch; the coordinator first verifies that no new NULL IDs appear
on the hub. Release N+1 activates X1d and may roll back only to release N, never a
build with partial ID minting. On restart, the identity seed refuses any NULL ID
above the frozen cutover instead of moving the cutover or silently repairing it.
A matching database snapshot restore is the last resort.

## Stored-ID readiness and source rollout

`NewDaemon` reads the environment file and seeds the opened database before
constructing the bus, manager, or other event producers. The explicit
`NewStored` branch verifies server/seeded environment equality and takes its
cutover from that persisted singleton. The background runner starts before
startup reconciliation. `/health.event_identity` exposes `ready` plus durable
progress coordinates/counts; unavailable reads omit the progress block and
remain unready. Health status is `starting` during a clean backfill, `degraded`
for unavailable
progress or an incomplete backfill with failed chunks, and `ok` after readiness
succeeds (historical failure counts are retained).
This metadata remains available with the stream API module disabled.

During initial backfill both snapshot and SSE endpoints return HTTP 503,
`Retry-After: 1`, `backfill_in_progress`, and `snapshot_required: false`.
Requests do not run backfill. Only complete readiness admits the persisted-ID
projection, so no response mixes generated and stored IDs and an oversized gap
cannot discard unbackfilled history. Failed chunks leave readiness false until
a successful resume. Default compatibility constructors remain unchanged.

When activated, `id` is the persisted opaque string, including both legacy
`<environment UUID>:<rowid>` and new `evt_…` values. The JSON `cursor` and SSE
`id:` remain the independent decimal log sequence. Producer sequence,
generation, origin time, process, scope, correlation, actor and classification
are preserved. Legacy rows without producer coordinates report the durable
`tether.events` log as their source; incomplete producer coordinates are an
error. An absent or unsupported actor is reported as the hub service with
`actor_verified: none` and `actor_role: system`, without inherited delegation.

During backfill, a NULL ID at or below the frozen cutover yields HTTP 503 with
`Retry-After: 1` and reason `backfill_in_progress` before streaming starts. If
already streaming, it yields a `gap` with the same reason,
`retry_after_seconds: 1`, and `snapshot_required: false`, then closes. Retry
from the last delivered cursor; do not take a snapshot that discards the pending
history. The complete global window is checked before session filtering.
A NULL above the cutover is an `event_id_invariant` error, never a generated ID
or skipped row. Existing purged/oversized-window gaps retain their snapshot
semantics.

### Atomic store producers

The application wires store-owned reaper, activity, channel-routing and
replacement events to `OutboxBus.PublishTx`. These events run the same
transaction-safe stages before insertion, using the caller's existing SQL
transaction. Session dimensions come from that transaction's session row;
actor and correlation still require verified context. Unknown generation and
actor stay absent. Database-reading stages remain excluded inside a transaction.
A failed event insert rolls back its business mutation; only committed rows
reach subscribers, with their original persisted IDs and sequence order. Bare
storage-only callers retain the persister primitive without a running bus.

### Sysop operator observations

Sysop enqueues its five existing operator observation kinds through
`OutboxBus.PublishTx`. The daemon drains their durable pending rows in log
order; Sysop does not clear pending or publish through a separate live stream.
IDs are minted once and stored, with producer app, private/local audit class,
known session dimensions, and the frozen environment identity when seeded.
Without a verified operator principal, actor and delegation remain absent.

The observation allowlist contains fixed source/action, bounded identifiers,
counts, and the known checkpoint-policy enum. Checkpoint summary/status and
policy status prose are excluded; the authoritative checkpoint request and
stored content remain unchanged. Cleanup observations carry counts rather
than a list of deleted sessions. Unknown fields are discarded before insertion.

The daemon action and this Sysop audit enqueue are separate cross-process
operations. A successful action can therefore lack an observation if Sysop
exits or its audit write fails afterward. Audit failure is logged without
request content and does not undo or report failure for an already successful
action. This change makes each enqueue transactional; it does not make the
preceding daemon action atomic with that enqueue.

### Activation evidence and rollback

Source tests exercise temporary databases only. They are not evidence that the
live hub has run the mint-capable release N, that its post-release writes have
zero NULL IDs, or that an acceptable rollback bundle is installed. The owner
owns that proof, the coordinated release N+1 activation, backup and deployment.
The historical 281-row estimate above is historical planning evidence, not a
current database measurement. Consumers must treat stored IDs as opaque and
resume from the log cursor.

Rollback requires a mint-capable build. Schema rollback is intentionally a no-op
for the additive identity metadata and insert guard: reverting source does not
delete, NULL or regenerate minted IDs, remove the frozen cutover, or move its
cursor. No destructive down migration is supplied in this forward-only
repository. A partial-mint rollback build is unsupported and its identity-less
writes are refused by the guard; startup also refuses NULL rows above the
cutover if an older schema/snapshot lacks that guard. A matching database
snapshot restore remains an owner-controlled last resort.

X1 events-table closure includes root transactional producers and Sysop's
outbox producer. Separate proxy-event, AI-audit and fabric tables retain their
own contracts. Raw runtime bodies remain excluded by the existing per-kind
sanitizer/admission ruling; this source wiring does not admit them or alter
retention policy. Existing degraded-mode lifecycle notices remain best effort.
