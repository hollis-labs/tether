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

The mapping is deliberately explicit:

| Durable source | Mesh projection |
| --- | --- |
| Event-table id | `seq`, decimal `Cursor`, and this source's `SourceSequence` |
| Environment id + event id | Stable envelope `ID` |
| Persisted timestamp/kind/payload | `Time`, unchanged `Kind`, unchanged `Payload` (`null` if absent) |
| Scope | `Process.scope` |
| Session id when recorded | `SessionID`, `Subject=urn:session:<id>` |
| Daemon or broker event without session | Environment `Subject` |
| Reporting source | `Actor=msg://service/tether/<environment_id>`, `Source.channel=tether.events` |

All currently persisted kinds follow this mapping: `session.state_changed`,
`session.turn_routed`, `session.turn_output`, `session.boot_dir_planted`,
`provider.session_lost`, `provider.permission_denied`, broker envelope events,
daemon lifecycle/sweep events, budget rejections, and new `session.request_status`.
No original caller attribution, provider/shim generation, causation/correlation,
or instance identity is invented when the old event row did not record it.
Generation `1` belongs to this projection, not the runtime's custody generation.
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
