# Trusted session context

The daemon verifies caller credentials before resolving call attribution.
`GET /auth/context` accepts no session selector. Its response has `verified`,
`source`, `principal_id`, `principal_kind`, `session_id`, `agent_urn`, `workstream_id`,
`launch_id`, `project_id`, and `logical_agent_id`. Empty fields are omitted.
Only a session principal whose session row exists receives `verified: true`.
An operator or service principal cannot select a session by claiming one.
Agent attribution additionally requires the current daemon binding to name
that exact session; missing or stale bindings leave `agent_urn` empty.

This verifies the credential-to-session association and reads daemon state;
it does not attest agent code or turn registry writes into authorization.
Address/scope enforcement and protection against stolen same-uid credentials
remain separate caller-identity phases. Attribution never grants authority.

The stdio adapter resolves context at forwarding/logging boundaries using its
own daemon credential, with a 200ms deadline and short per-adapter caches.
Lookup failure leaves the content
call available without a trusted context stamp; legacy correlation follows its previous behavior. A daemon-owned adapter can resolve
the same context directly from its verified request principal. `--session`,
environment values, incoming headers and `_meta` are claims, not selectors.

`proxy_events` exposes `attribution` and `claimed_session_id`. The daemon
recomputes attribution at receipt and ignores a client-supplied `attribution`
object. Verified session callers override the legacy top-level `session_id`;
verified operator/service callers clear it. For compatibility, anonymous
observe/off traffic retains its old top-level session claim, also recorded
as `claimed_session_id`, with `attribution.verified: false`. Consumers must
use the attribution object when they require verified identity. Events emitted
on the daemon bus carry the same attribution as the persisted row. Claims
cannot overwrite it. Context describes state at each receiving boundary;
workstream/binding changes between forwarding and receipt can change a snapshot.

Migration 0040 adds `attribution_json` and `claimed_session_id` to the existing
table. Previous rows default to unverified, with no inferred backfill. These
fields share the row's existing age retention and purge audit receipt; there
is no separate attribution table or additional retention setting.

Outbound `_meta.tether.provenance` retains schema 1 and its exact existing
shape: `schema_version`, `session_id`, optional `workstream_id`. For verified
calls, those two identity fields come from the resolved snapshot. Unverified
calls retain the previous configured-session/workstream lookup behavior. This
legacy envelope is correlation only and never proves identity; retaining it
preserves existing receivers' workstream association.

The separate reserved `_meta.tether.context` schema 2 contains `schema_version`
and the full verified context, including `verified: true` and `source: daemon`.
It is omitted for unverified calls. Both inbound envelopes are always stripped
before stamping. Reserved `X-Tether-*` and `X-Forwarded-User-*` metadata keys
are stripped, including case variants. Ordinary arguments remain unchanged.
Receivers can adopt schema 2 independently; they must trust it only when their
own authenticated boundary establishes the sender. A version or verification
field is not authentication. Stdio upstreams serve one session and require no
forwarded-user HTTP headers.

Trace context is independent of identity. Existing `_meta._traceparent` and
`_meta._tracestate` propagation continues; legacy argument extraction remains
supported. Trace metadata never establishes a principal. Arbitrary incoming
`X-Forwarded-For/Host/Proto` values establish neither identity nor a trusted
reverse-proxy boundary, and are not relayed to unrelated upstreams.

Production use starts with 0539's daemon transport mount/cutover. This hooks
into its shared-owner foundation; existing worker-owned stdio proxies continue
using their existing upstream credentials. Daemon-owned HTTP/SSE egress opts
into forwarded identity with an explicit catalog entry:

```yaml
id: upstream
transport: http # or sse
url: https://upstream.example/mcp
allow_unconfined_remote: true
proxy_service_token_file: /absolute/operator-owned/upstream-service.token
```

The upstream's operator/admin provisions a dedicated Tether-proxy service
principal and issues its bearer credential. Tether does not create that
upstream principal or reuse a session bearer/operator token. Store the
credential in a current-user-owned regular file with exactly mode 0600. The
path must be absolute; it is never expanded from worker environment variables.
The reader checks the opened inode, refuses a final symlink or special file,
and bounds the file at 4096 bytes. Content must be one opaque bearer value,
with optional surrounding whitespace, never a header or multiline secret.
An entry combining `token` and `proxy_service_token_file`, or using the service
field for stdio, is refused. Disabled entries do not read their service file.

Only the daemon's upstream owner loads this file, into a private in-memory
entry used by existing error redaction. The authored catalog and worker
ENV/boot files receive no service secret. A runtime loads the credential once;
a changed credential takes effect when that owner is recreated. No automatic
service-token mint/rotation, install, restart or identity-mode change accompanies
this change. Operator rollout still owns provisioning and cutover.

An entry-aware HTTP client factory keeps separate policies even for entries
sharing a URL. Each request is cloned, caller-controlled `X-Tether-*` and
`X-Forwarded-*` headers are stripped case-insensitively, and Authorization is
set only to this entry's upstream service bearer. Redirects are refused and
the transport rejects requests to another origin. Ordinary trace metadata
remains independent of identity.

Actor headers require all of: an admitted session principal, a daemon-resolved
verified snapshot matching its principal/session, and the private marker set
only around the actual SDK `CallTool` POST. The preceding Ping/reconnect,
initialize, background GET, refresh and probes carry service authentication
without actor headers. Operator/service principals remain identifiable in
telemetry but never inherit a claimed session or actor headers. The upstream
must pin the configured service principal before trusting these headers;
Source/Verified strings and ordinary bearer authentication alone establish no
forwarded-user trust. Agent attribution confers no operator-confirm authority.

| Outbound header | Resolved snapshot field |
| --- | --- |
| `X-Forwarded-User-Id` | `session:<session_id>` |
| `X-Tether-Session-Id` | `session_id` |
| `X-Tether-Agent-Urn` | `agent_urn`, only with a current binding |
| `X-Tether-Workstream-Id` | `workstream_id` |
| `X-Tether-Launch-Id` | `launch_id` |
| `X-Tether-Project-Id` | `project_id` |
| `X-Tether-Logical-Agent-Id` | `logical_agent_id` |

Empty optional fields are omitted. Each field is at most 512 visible ASCII
bytes; an unsafe/oversized field suppresses actor headers while preserving the
content call and service authentication. This does not mutate shared defaults
or store one caller's identity on an upstream connection. The schema-2 context
in the MCP envelope retains the same resolved snapshot as attribution.

For service-authenticated SSE, handshake cancellation bounds dialing and
response headers. After headers arrive, the SDK owns the long-lived response
body; closing it releases the stream. This prevents the handshake timeout from
immediately closing a successfully initialized SSE connection. Stream GETs
still carry no actor attribution.

Stdio attribution resolution happens only at forwarding/logging boundaries;
local native tools do not look up attribution merely to execute. Successful
principal snapshots are cached for five seconds per adapter; anonymous results
and lookup failures for three seconds, with a 200ms lookup deadline. These are
attribution caches, never authorization caches. Revocation remains checked by
the daemon on every authenticated request. A cached forwarded snapshot can lag
a changed binding or workstream; the receiving daemon resolves its durable row
again. Snapshot equality is semantic field equality: `tether.context` also has
`schema_version`, and its JSON key ordering can differ from the persisted row.

Verified `GET /auth/context` and `POST /proxy/events` authenticate normally but
skip duplicate `identity_audit` observation rows. The event ingest already
persists attribution in `proxy_events`. Invalid credentials on these routes
remain audited. Adapter telemetry for verified operator/service principals
clears the session id, preserving any configured session flag only as a claim,
to match the daemon row.
