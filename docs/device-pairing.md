# Device pairing and remote scopes

ADR 0062 and CW-20261009-0070 use device credentials for protected remote
routes. The local listener keeps its existing identity policy. The optional
remote listener always enforces device identity and scopes; a locally valid
operator, service, session or interactive credential is refused remotely.
Health and the minimal environment descriptor remain public. Remote MCP stays
absent. Pairing changes no provider credential or native shim custody boundary.

## Local grant, remote exchange

An operator creates a one-use grant through the accepted local Unix socket:

```sh
tether pair --scope read,operate --ttl 5m --label worker-client
```

The command prints the secret code once; `--json` includes its id, scopes and
expiry. Protect that output. It loads the existing operator credential and
refuses a TCP daemon address before reading that credential. It does not open
the database, mint an operator, change config or enroll a worker.

`POST /auth/pair` requires the verified operator on the accepted Unix connection.
The default grant scope is `read`, default TTL is five minutes and maximum TTL
is one hour. Scopes must be an explicit nonempty subset of the five below.
An optional `key_thumbprint` reserves an exact exchange binding; it does not
perform DPoP or prove possession of a private key. Grant revocation is local-only
`POST /auth/pair/revoke` with its id.

The remote client sends `POST /auth/pair/exchange` with `code`, optional `scopes`
and optional `key_thumbprint`. Omitted scopes keep the grant's scopes; supplied
scopes can only narrow them. The request requires `Tether-Protocol: 1` and the
configured exact Host/Origin allowlists, with no bearer needed. Its body is
limited to 4 KiB. A listener-wide bucket permits a burst of ten exchanges and
refills one every six seconds; it does not allocate buckets from forwarded IPs.

Every syntactically valid grant lookup uses one conditional update requiring
unconsumed, unrevoked, unexpired state and the requested scope subset, then mints
the device in that same transaction. A mint failure rolls back consumption.
Unknown, expired, revoked, replayed, wrongly bound and insufficient grants return
the same 401 refusal without a reason lookup. Malformed bodies return 400 and
rate limiting returns 429. Database operations are not claimed constant-time.

The response contains the new device id and its `tth_` token once. Codes and
tokens contain 256 random bits; the database stores only their SHA-256 hashes.
Store the device token privately on the client and use it as an Authorization
bearer. Redirects must never carry it to a different endpoint. Identity responses
use `Cache-Control: no-store`.

## Independent scopes

| Scope | Remote route admission |
|---|---|
| `read` | Environment reports and streams, sessions, messages, catalog metadata and ordinary observation |
| `operate` | Catalog launches, turns, stop/resume/checkpoint, messaging and team operations |
| `terminal` | Raw PTY input, resize and raw attachment |
| `maintain` | Settings, registry maintenance, diagnostics, retention/repair and custom launch policy |
| `admin` | Device list and device revocation |

These scopes are independent: `admin` does not imply any other scope and `*`
is not a device scope. `internal/api/remote_scopes.go` is the declarative table
for every remote route and payload case. Unlisted paths, methods and RPC methods
are refused. Structural coverage checks require newly registered API routes to
declare policy; there is no GET/read or mutation/operate fallback.

Creating a session with an agent file, inline agent, override, boot-profile file
or injected native files requires both `operate` and `maintain`. Ordinary
catalog launches require `operate`. Session bootstrap, proxy telemetry and
runtime binding mint/renew/revoke remain local custody operations, regardless of
device scope. Module switches still decide whether an authorized route exists.

Route admission does not grant execution or MCP authority. Native-only resume
still requires the existing independent current launch authority (`session.write`
or `*`) and the historical credential/MCP ceiling. A paired device with `operate`,
even with all five scopes, reaches that route but receives
`native-only resume unavailable: independent current launch authority required`
with HTTP 409 before allocating a destination or executing its runtime. An
ordinary device-initiated launch inherits no legacy
`session.write`, `message.write` or `catalog.write` scope in its child credential;
the five independent device scopes are not translated into worker/MCP grants.
The ordinary child's seven-day expiry is not capped to the parent's device expiry.
`CreatedBy` records provenance; parent device revocation does not revoke that child
credential. Device request/stream cancellation does not establish child revocation.
These preserved limitations require a separate authority decision. Ordinary
pairing success does not prove every `operate` execution path is compatible.

Device id namespaces launch/resume body keys and routing-reply/team
`Idempotency-Key` headers. The verified id is used, never `as`, `from`, display
name or remote address. Local idempotency behavior is retained.

## Lifetime, inspection and revocation

Devices expire after thirty days. A valid current device can call
`POST /auth/renew` with optional narrowed scopes to extend expiry thirty days
from now, retaining its token. Expired or revoked tokens cannot renew; a stale
scope snapshot cannot restore scopes removed by a concurrent renewal. Scope
narrowing cancels requests and streams authorized by the old scope set.

```sh
tether auth list
tether auth revoke msg://device/DEVICE_ID
```

These CLI commands use the local operator Unix socket. Remote devices need the
independent `admin` scope for `GET /auth/devices` and `POST /auth/revoke`.
Listing returns id, label, scopes, creation/expiry/revocation, last use, parsed
peer IP and a bounded user agent. Tokens and hashes are absent. Remote use and
audit metadata commit before dispatch; audit failure refuses the request.

Revocation cancels current requests and streams immediately in the same Store.
Other Store instances observe committed expiry, revocation or scope changes on
a one-second polling interval, with a bounded verification timeout. Cancellation
also interrupts a stream blocked on writing to its peer. It does not stop the
hosted session or change retained shim authority.

## Acceptance limits

This source slice uses only disposable databases, Unix/TCP listeners and
synthetic credentials. Actual worker pairing, enrollment and live credential
migration are separate acceptance work. No live unit, catalog, credential,
grant or daemon configuration is changed by source completion.

A2A's inner binding-bearer authentication remains in place. A device token
cannot satisfy a different binding bearer, while that legacy bearer fails the
outer device-only gate. The synthetic refusal witness preserves this unsupported
integration for CW-20261009-0074. That task also owns federation peer-secret
resolution and Subscribe's missing `as` forwarding.

The environment is single-owner: `read` sees all environment data. Per-principal
filtering is required before 1.0. Same-uid database or token-file access can
defeat these checks; database hash storage does not create read isolation.
