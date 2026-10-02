# Verified caller identity

CW-20260930-0253 phase 1 starts with daemon-verified bearer identity. The approved
record is Tesseract workspace item `01M3TVNJ02SD1C3NRXBF4FF1CC`; its section 8
recommendations are approved by Chrispian and lead.

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
`service`, `interactive`), display, scopes, optional session id, permitted
addresses, creator, creation time and optional revocation/expiry times.
Tokens contain 32 cryptographically random bytes with a `tth_` prefix. Only a
SHA-256 hash is stored, with a unique indexed hash lookup. Database lookup
timing is not claimed to be constant-time. Multiple tokens may represent one
principal; revocation by principal id revokes all its token rows.
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

Each launched session in observe/enforce receives a fresh session principal and
`tth_` credential. A verified parent is recorded as its creator; worker scopes
are intersected with the parent's grants, and the address is that session's own
`msg://session/local/<id>`. Anonymous observe-mode launches have no verified
creator. Session tokens enter the runtime and planted MCP server via
`TETHER_TOKEN` environment entries, never argv or the stored launch plan. Native
MCP configuration files necessarily contain that environment entry inside the
session's boot directory. Same-uid read isolation remains a separate boundary.

Migration 0037 revokes session principals transactionally when the session ends
as completed, failed or killed, or is deleted; minting against missing/terminal
sessions is refused. Only one unrevoked token may exist per session, so concurrent
launch attempts cannot mint competing credentials. A launch that fails after mint
revokes only its own token; terminal transitions revoke all of the session's tokens.
A resumed session has a new session id and token. Identity-off launches mint no
principal; their MCP adapter retains the legacy nonsecret presence marker.
Existing running sessions are not retrofitted.

The client library's credential options have their own task/PR and require a
lead-chosen release before consumers adopt them. No token introspection endpoint
is required.

Phase 2 covers message `from` stamping, mailbox ownership, per-principal
idempotency, AI caller stamping, route-scope policy and launch-plan read
restrictions. Phase 2 must also reconcile the operator messaging address
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
