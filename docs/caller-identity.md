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
`/health` remains open and does not generate identity observations.

`enforce` is an explicit opt-in authentication gate: an absent or invalid
credential returns 401, and identity/audit availability failures return 503.
Phase 1 does **not** enforce route scopes or sender/mailbox ownership. It does
not turn on enforcement anywhere. Non-loopback TCP binds require explicit
`enforce`; this is an authentication condition, not transport encryption.

## Principals and credentials

The `principals` table holds a principal's id, kind (`operator`, `session`,
`service`, `interactive`), display, scopes, optional session id, permitted
addresses, creator, creation time and optional revocation/expiry times.
Tokens contain 32 cryptographically random bytes with a `tth_` prefix. Only a
SHA-256 hash is stored, with a unique hash lookup and constant-time comparison.
Revoked and expired credentials cannot verify. Raw tokens are not included in
principal JSON, audit receipts or events.

Only daemon startup bootstraps the operator credential. Its file is
`run/operator.token` next to the selected catalog directory (normally
`~/.tether/run/operator.token`). An existing credential must verify against the
selected state database as the operator principal. Readers require a regular
file owned by the current user with exactly 0600 permissions; symlinks and
special files are refused. Creation is exclusive and never overwrites a file.
The directory is created with 0700 permissions when absent.

A loose-permission, malformed, revoked or mismatched file fails startup; it is
not silently repaired or adopted. Deleting the file cannot rotate an existing
principal. A failed/interrupted first bootstrap may leave a file/database
mismatch and requires explicit operator recovery. Preserve the token file and
state database together in backups. Ordinary clients never create this file.

## Attribution and retention

Each non-health request in observe/enforce writes an `identity_audit` receipt
with timestamp, verified principal/session ids when available, mode,
authentication result, method and route family. The daemon also publishes
`identity.observed` with the same metadata through the durable event bus.
Only the first route segment is recorded; headers, bodies, query strings and
individual resource ids are excluded. Token-looking or oversized route names
are redacted. Observation failures are logged without database error contents.

| Table | Retention policy |
|---|---|
| `principals` | Indefinite, including revocation records; no automatic principal purge |
| `identity_audit` | Indefinite; independent of event-history expiry |
| `events` (`identity.observed`) | The daemon's shared event-history retention window |

This core PR does not yet deliver tokens to existing CLI/MCP clients or mint
session credentials. Those are the second phase-1 PR. The client library's
credential options have their own task/PR and require a lead-chosen release
before consumers adopt them. No token introspection endpoint is required.

Phase 2 covers message `from` stamping, mailbox ownership, per-principal
idempotency, AI caller stamping, route-scope policy and launch-plan read
restrictions. Phase 3 covers live enforcement rollout and consumer adoption.
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
