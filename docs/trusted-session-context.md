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

The approved HTTP forwarding contract is a separate part B after the 0539
daemon egress seam lands: only daemon-owned egress carrying an explicitly
configured per-upstream Tether-proxy service credential may stamp
`X-Forwarded-User-Id: session:<verified-id>`. Provision that credential through
the upstream's operator/admin flow, storing it in a current-user-owned regular
0600 service token file. The planned catalog field `proxy_service_token_file`
will use the existing fail-closed owner/mode/no-symlink reader. It is not yet
accepted by this part A. The upstream pins that service principal before
honoring actor headers; an ordinary bearer token does not imply this trust.
Never send the session bearer upstream or put the service credential in a
worker's environment/boot files. Per-call identity must not become shared
connection/default headers. Forwarded agent identity never confers operator
confirmation authority. No service credential is automatically provisioned,
and no live configuration, restart or identity-mode change accompanies this PR.

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
