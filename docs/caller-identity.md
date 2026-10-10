# Verified caller identity

The local listener retains the CW-20260930-0253 phase-1 policy. Optional remote
access uses [paired device credentials and route scopes](device-pairing.md),
independently of local identity mode.

CW-20260930-0253 phase 1 starts with daemon-verified bearer identity. The approved
record is Tesseract workspace item `01M3TVNJ02SD1C3NRXBF4FF1CC`; its section 8
recommendations are approved by Chrispian and lead.

MCP provenance and tool-call attribution use the [trusted session context](trusted-session-context.md)
contract (CW-20261001-0543): credential-derived context is separate from caller
claims, and attribution does not itself grant permissions.

```yaml
identity:
  mode: observe
```

An omitted mode and newly seeded catalogs use `observe`. `off` skips identity
verification and observation. `observe` verifies a supplied `Authorization:
Bearer tth_…` credential, puts the verified principal in request context, and
records attribution without rejecting requests. Missing, malformed, unknown,
revoked or expired credentials remain permitted, as do verifier/audit failures.
`/health` remains open and does not generate identity observations. Anonymous
requests do no identity audit work. A2A (`/a2a` and its subtree) is explicitly
exempt in phase 1: that adapter owns its independent bearer authentication.
Modes are case-normalized; invalid modes and non-local observe/off binds are
rejected at daemon startup and flagged by doctor, not by shared configuration
resolution used for stop/status/MCP clients.

`enforce` is an explicit opt-in authentication gate: an absent or invalid
credential returns 401, and verifier availability failures return 503. Audit persistence remains best-effort.
Phase 1 does **not** enforce route scopes or sender/mailbox ownership. It does
not turn on enforcement anywhere. Non-loopback TCP binds require explicit
`enforce`; this is an authentication condition, not transport encryption.

## Principals and credentials

The `principals` table holds token rows: a unique row id and a non-unique
`principal_id` identify the credential and stable principal separately. Each
row holds kind (`operator`, `session`,
`service`, `interactive`, `device`), display, scopes, optional session id, permitted
addresses, creator, creation time and optional revocation/expiry times.
Tokens contain 32 cryptographically random bytes with a `tth_` prefix. Only a
SHA-256 hash is stored, with a unique indexed hash lookup. Database lookup
timing is not claimed to be constant-time. Multiple tokens may represent one
principal; device ids are unique to one credential. Revocation by principal id
revokes all its token rows.
Revoked and expired credentials cannot verify. Raw tokens are not included in
principal JSON, audit receipts or events.

Only daemon startup bootstraps the operator credential. Its file is
`run/operator.token` next to the selected catalog directory (normally
`~/.tether/run/operator.token`). An existing credential must verify against the
selected state database as the operator principal. Readers require a regular
file owned by the current user with exactly 0600 permissions; symlinks and
special files are refused. Creation is exclusive and never overwrites a file.
The directory is created with 0700 permissions when absent.

Readers fail closed on loose-permission, malformed, revoked or mismatched files.
In **observe**, a bootstrap mismatch logs a warning and startup continues with
operator credentials degraded; health and doctor report that state. Other
principal verification remains available. **Enforce** refuses startup. Files are
never silently repaired or adopted; deleting the file does not rotate a prior
principal. Ordinary clients never create this file.

Recovery: keep and restore the **matching DB and token-file backups together**.
A DB-only restore, lost run directory or interrupted first bootstrap can leave
credentials degraded. Continue in observe while recovering the matching pair;
do not enable enforce until doctor reports credentials available. Atomic
operator rotation/recovery is tracked separately in CW-20261002-0003; this PR
adds no live recovery command. Never remove principal records or substitute an
unrelated token as an implicit repair.

## Attribution and retention

Requests that present an Authorization credential enqueue an `identity_audit`
receipt with timestamp, verified principal/session ids when available, mode,
authentication result, method and route family. The 256-entry queue is
non-blocking: overflow/shutdown drops and persistence failures are counted in
health and doctor. A worker persists accepted receipts independently of request
lifetime. Auditing is best-effort, including in enforce mode; queued observations
can be lost at shutdown. Anonymous traffic is not queued.

There is **no per-request bus event**: event waiters and SSE watches stay idle
unless an application event occurs. Only the first route segment is recorded;
headers, bodies, query strings and individual resource ids are excluded.
Token-looking or oversized route names are redacted. Credentials never appear
in audit metadata or diagnostic counters.

| Table | Retention policy |
|---|---|
| `principals` | Indefinite, including revoked token rows; no automatic purge |
| `identity_audit` | Shared `daemon.events_retention` window: default 90 days; false or days < 1 disables; deletions write durable retention receipts |

## Clients and session credentials

CLI and MCP requests prefer `--token-file`, then `TETHER_TOKEN`, then the selected
catalog's sibling `run/operator.token`. A missing default file permits anonymous
requests in observe/off mode; a missing explicit file or insecure existing file
fails closed. Credentials travel as an Authorization header, including streams.
Authenticated clients refuse redirects to another origin. Legacy `mcp --token`
and `TETHER_MCP_TOKEN` are only adapter presence checks, not daemon credentials.

A normal launch in observe/enforce receives a fresh session principal and
`tth_` credential. A verified parent is recorded as its creator; worker scopes
are intersected with the parent's grants, and the address is that session's own
`msg://session/local/<id>`. Anonymous observe-mode launches have no verified
creator. Session tokens enter the runtime and planted MCP server via
`TETHER_TOKEN` environment entries, never argv or the stored launch plan. Native
MCP configuration files necessarily contain that environment entry inside the
session's boot directory. Same-uid read isolation remains a separate boundary.

Session tokens expire after seven days as a backstop; terminal revocation remains
the primary lifetime control. Long-running token renewal is future work, so phase 1
continues to use observe. Retrying a still-created session atomically revokes any
stale credential before minting a replacement; a running session is never rotated
by this launch path.

Migration 0037 cleans duplicate and terminal/orphan credentials before creating
the active-token uniqueness index, and revokes session principals transactionally when the session ends
as completed, failed or killed, or is deleted; minting against missing/terminal
sessions is refused. Only one unrevoked token may exist per session, so concurrent
launch attempts cannot mint competing credentials. A launch that fails after mint
revokes only its own token; terminal transitions revoke all of the session's tokens.
A resumed session has a new session id and token. Identity-off launches mint no
principal; their MCP adapter retains the legacy nonsecret presence marker.
Observe-mode mint failures warn and continue anonymously, with inherited bearer
credentials cleared in both runtime and MCP environments. Enforce-mode mint
failures stop the launch. Preparation is serialized per session, including
degraded launches, so concurrent attempts cannot overwrite the winning boot config.
Existing running sessions are not retrofitted.

The client library's credential options have their own task/PR and require a
lead-chosen release before consumers adopt them. No token introspection endpoint
is required.

The remote listener now enforces device route scopes and namespaces launch,
resume, routing-reply and team idempotency keys by the verified device id.
Local keys and local authorization retain their existing behavior. Remaining
phase-2 work covers message `from` stamping, mailbox ownership, AI caller
stamping and launch-plan read restrictions. Phase 2 must also reconcile the operator messaging address
`msg://user/local/me` with its stable principal identity. Phase 3 covers live enforcement rollout and consumer adoption.
Observe-mode attribution does not prevent same-uid credential theft: meaningful
impersonation protection also requires read isolation (CW-20261001-0263), along
with control-plane write protection (CW-20260930-0237). Pre-switch sessions must
relaunch when enforcement is enabled. Interactive agents get a distinct
interactive principal, not the operator credential.

## Cutover

No install, live token creation, restart or mode change is performed by this
change. Chrispian owns cutover. Back up the state database first, keep default
`observe`, and review attribution before any separately approved enforcement
flip. Route-scope and address authorization must be assessed with phase 2.

## Remaining phase-2 client plumbing

The daemon-status HTTP probe still uses a direct HTTP caller without bearer
credentials. It remains available in observe/off; explicit enforce can reject
it. Authentication for that path remains phase-2 work. MCP event forwarding
now uses the credentialed daemon client so the daemon can resolve attribution.

## Proxy credentials

Session proxies (`--session` or the planted `TETHER_MCP_TOKEN` marker) never use
the operator-file fallback when their own token is empty. Identity-off and degraded
launches remain anonymous even if identity is later enabled. The sessionless
`boot-exec` proxy is explicitly anonymous and retains only the adapter presence
marker, rather than inheriting the operator fallback. Ordinary operator CLI calls
still use the documented fallback; explicit token files/environment remain first.

Stdio upstream processes do not inherit any `TETHER_*TOKEN` variable from the
proxy. An upstream catalog entry may deliberately supply its own token variable
in `env`; that explicit configuration is retained.

## Device-only remote authentication

CW-20261009-0070 replaces CW-20261009-0068's generic non-operator remote
admission with the scoped device-token authentication chosen by ADR 0062.
Protected remote routes refuse operator, service, session and interactive
credentials, even if locally valid. Health and the minimal descriptor remain
public; the bounded POST `/auth/pair/exchange` is the only unauthenticated
credential bootstrap exception. Local operator and session behavior is retained.
Remote device use writes its audit and metadata transactionally before dispatch;
an audit failure returns 503. Device revocation cancels requests and open streams
by the current authenticated credential, independently of `as`, `from` or labels.
See [device pairing](device-pairing.md) for lifetimes and scope policy.
