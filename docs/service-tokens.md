# Service tokens

A service token is the credential of an app or workload (Cerberus, Torque,
Tachyon, ...). It is an Ed25519 JWT issued by Tether's one issuer, with
`kind: "service"`, the principal `msg://service/<app>/<env>` as `sub`, one
audience and explicit scopes of the form `<audience>.<verb>`. It is the third
token class beside the operator's and a paired device's (security direction v1).

## No expiry, revoked by deny-list

A service token has no expiry in the sense that matters: its `exp` is ten years
out (go-auth rejects `exp: 0`, so "no expiry" is implemented as a very distant
`exp`). It is ended by **revoking** it: its `jti` goes on the deny-list until its
`exp`. A verifier stops accepting it within one poll interval (30 s in the library)
after a successful poll of the deny-list. If polling fails, the verifier keeps the
last list it learned: it still rejects every revocation it already knew, but a
newly revoked token can keep being accepted until a poll succeeds.
The 60 minute issuer cap applies to every other token and is not changed; only
`kind: "service"` escapes it, and only through this issuing path. A service token
cannot mint other tokens.

## Issuing, listing

Only the operator, over the local socket, issues service tokens (dev posture:
loose; tightening plugs in here later). The token is shown once.

```sh
# Cerberus may publish events to the hub (audience tether, scope tether.events)
tether service-token issue --app cerberus --scope events --label "agent-os cerberus" \
  --out-file /path/to/cerberus.token         # new 0600 file; the token is not printed

# An app that verifies its own audience
tether service-token issue --app tachyon --audience tachyon --scope tachyon.approvals_enqueue

tether service-token list [--json]           # metadata only, never a token
```

HTTP: `POST /auth/service-tokens` (body `app`, `env`, `audience`, `scopes`,
`label`, `allow_wildcard`) and `GET /auth/service-tokens`. Both are local-only:
a remote caller, a device, a service, an agent or an operator-minted narrow token
is refused. The issuer logs ids and scopes, never the token. Its metadata (jti,
principal, audience, scopes, label, issuer, times; never the token) is kept in the
issuer's state file (0600) and survives a restart.

## The dedicated service signing key

Service tokens live ten years, so they are signed by their own key, not by the
15-minute session/exchange key. Its kid starts `svc-`, it is created on first use,
and it lives with the other signing keys (the issuer's 0600 key files in a 0700
directory; never logged, never in `keys.json`, `jwks.json` or the state file). The
JWKS publishes its public half like any other key, so external verifiers need no
change.

- **The JWKS publishes exactly the service keys live tokens need.** Consumers
  built on go-auth (Torque, Tesseract, Tachyon producers, ...) *replace* their key
  set on every JWKS refresh; they do not merge. So `/.well-known/jwks.json` always
  contains the active `svc-` key and every rotated-out `svc-` key that a live
  (unrevoked, unexpired) service token record still names, so a ten-year token
  keeps verifying across any number of ordinary key rotations and service key
  rotations. A rotated-out key leaves the JWKS as soon as the last record on it is
  revoked (revoking prunes at once) or, for an expired record, at the next hourly
  maintenance pass. Tests use the real go-auth `JWTVerifier` refreshing after a
  session-key rotation and prune.
- Ordinary key rotation and the hourly prune never touch it. A service key is
  deleted only when it has been rotated out **and** no live (unrevoked, unexpired)
  service token record names it. The active service key is never deleted.
- `tether service-token rotate-key` makes a new service key active. Tokens signed
  by the old key keep verifying (the old key stays published). It reports which
  live records are still on an older key and need re-delivery.
- `tether service-token rotate-key --reissue --out-dir DIR` also mints a
  replacement for every such record under the new key (same principal, audience,
  scopes, label; the record notes the jti it replaces) into one new `0600` file
  each in `DIR` (never printed). Nothing is redistributed and the old tokens are
  not revoked: deliver the new ones, then `tether service-token revoke` the old.
- `tether service-token revoke --all [--app A]` is the emergency stop: every live
  service token (or every one of an app, any env) goes on the deny-list.

**Blast radius.** The service key can mint any service token. If it leaks, an
attacker can mint tokens for any service principal and any audience until the key
is rotated and the old tokens are revoked (`rotate-key`, then `revoke --all`). The
dev mitigation is the tailnet boundary around the hub, the 0600/0700 key
directory, and that revoking is immediate in Tether and reaches
other verifiers within one poll interval after a successful poll (a verifier
that cannot poll keeps accepting a newly revoked token until it can). A leaked **token** is limited to its own scopes and is revoked on its
own.

## Revoking

```sh
tether service-token revoke <jti>               # one token
tether service-token revoke --app cerberus      # every live token of that app (--env, default dev)
```

HTTP: `POST /auth/service-tokens/revoke` with `{"jti": ...}` or `{"app": ..., "env": ...}`
(local operator only; revoking something unknown or already revoked is a 404).
The `jti` goes on the deny-list **until the token's own `exp`**, so a service token
is never pruned from the list at the 65 minute session horizon, and the listing
keeps the revocation time. Tether's own verifier shares the list and refuses the
token at once; any other verifier stops accepting it within one poll interval
(30 s by default, the library's) after a successful poll. A verifier that cannot
poll keeps its last known list: it still rejects what it already learned, but a
newly revoked token can be accepted until a poll succeeds.

## The deny-list is public (minimal)

`GET /auth/denylist` (and `HEAD`) needs no credential, no scope and no protocol
header, on the local and the remote listener. It returns exactly three arrays of
opaque ids, `jti`, `sub` and `sid`, with an `ETag` and `If-None-Match` support;
never a token, never a label, never a scope. A verifier that cannot read the list
cannot honor a revocation, and nothing in it is secret. **Writing** it
(`POST /auth/denylist`) is unchanged and needs `tether.admin` (or the operator
over the local socket). Earlier, reading it needed `tether.maintain`; a poller that
still sends a bearer is unaffected.

## A session requests its own app's token (CW-20261020-0036)

An agent session can ask for a service token for **its own app**, so an app that
launches agents (Torque) does not need the operator to hand-issue each token.

- **The app lives on the session.** `sessions.app` (migration 0098) is stamped by
  the hub when the row is created and has no update path. `NULL` means no app, and
  a session with no app cannot request. Only the verified local operator sets it,
  with `POST /sessions {"app": "<id>"}`; an agent or session asserting it gets 403
  `app_not_permitted`, and any other caller has it dropped and audited
  (`app_dropped`). The value must have the service-principal id shape
  (lowercase, `[a-z][a-z0-9-]{0,31}`) **and** be a registered producer in the event
  catalog that `POST /events` binds apps to (`producers.json` in
  `libs/harness/core/.../runtimeevents/catalog`); otherwise 400 `app_not_registered`.
  The operator credential is the root of trust until a launcher has its own service
  identity (CW-20261019-0047).
- **Children inherit and cannot widen.** A session created with a `parent_session_id`
  (resume) takes exactly its parent's app, or none; a team session takes its parent's
  app when it is linked (`LinkTeamSession`); a replaced team session copies it.
  Sessions made by `POST /sessions/bootstrap` (external, nothing launched) get no app.
- **The request.** `POST /auth/service-tokens/request` on the **local** listener
  only (the remote listener refuses it). The caller must be a verified session or
  agent credential with a session id; the hub loads that session's row itself (the
  body names no session, app, env or audience, and an unknown field is a 400). It
  issues only `msg://service/<session.app>/dev`, audience `tether`, scope
  `tether.events` (the only scope a session may request; none named means
  `tether.events`). An ended session or one with no app is refused.
- **Posture.** `identity.service_request` in `global.yaml`: `auto` issues at once and
  logs; anything else, including leaving it out, records the request and answers
  `202` without issuing (the approval flow is CW-20261015-0052). The daemon logs
  the active mode at start. A dev hub that wants hands-off issuing sets `auto` in its
  `global.yaml` explicitly (an operator change; nothing here edits live config).
- **Cap and audit.** At most 5 live tokens per (app, requesting session), inside the
  global bound of 256. Each request writes an `identity_audit` row (`service_token_request
  app=... decision=issued|pending|refused jti=<first 8>`) with the requester and
  session, never the token.
- **Same token.** The token is signed by the same `svc-` key, recorded the same way
  (plus `requested_by` and `requested_session`), listed by `service-token list` and
  revoked like any other, including by `revoke --all --app`. `Exchange` still refuses
  agent and session parents; `RequestService` takes no operator principal.
- **Not yet.** An agent token (`TokenID` set) is refused by the scope gate when
  `local_api` is `enforce` (a `local-only` route has no scope an issued token holds);
  stored session credentials and the dev `observe` posture work. A scope for it is a
  decision for when enforce flips. The token is returned once in the response; there
  is no `out_file`.

## Scope rules

- Explicit verbs by default. `<audience>.*` is accepted only with
  `--allow-wildcard` (dev), and each use logs a warning.
- For audience `tether`, a service token may hold only `tether.events`,
  `tether.read` and `tether.messages`. Nothing that administers Tether, launches
  sessions or edits the catalog.
- The audience and the scope prefix must match; reserved enrollment audiences are
  refused.
- At most 256 live service tokens; revoke unused ones.

## Reaching the hub from another host (remote listener)

The hub's remote listener (behind Tailscale Serve, the URL the workers already
use) accepts a service token on **exactly one route: `POST /events`**, so a
service on agent-os or worker-1 publishes events with its own token and no device
pairing. Paired worker devices keep working on the same route.

How it is checked, in this order, all required:

1. The request is exactly `POST /events`: no query string (not even a bare `?`),
   no trailing slash, no subpath, no other case, no encoded or doubled slash, no
   other method. Anything else, with the same token, is refused 401 as before.
2. The bearer is a three-part JWT (a `tth_` device credential never takes this
   path, and a JWT never takes the device path).
3. The token verifies like it does for any external app: Ed25519 signature
   against the issuer's keys, `iss` pinned, `aud` = `tether`, `exp`, and the
   shared deny-list (`jti`, `sub`, `sid`), so a revocation applies at once.
4. `kind` is `service`, the principal is `msg://service/<app>/<env>`, a `jti` is
   present, and that `jti` is a **live record of IssueService** (not revoked, not
   expired): a token the issuer minted any other way is not accepted here.
5. The scope `tether.events` is granted (else 403 `scope_required`).

Then the ingest rules apply: the service is bound to its app (an event naming
another app is 403 `app_mismatch`), the actor is the service principal, and the
per-principal rate limit applies like for devices. The `Tether-Protocol` header is
not required for this route. The whole path exists only while
`daemon.event_ingest.enabled` is on and this daemon is the issuer; otherwise a
JWT is refused like any other non-device bearer.

Failures: bad, unknown, expired, wrong-audience or revoked token -> 401; missing
scope -> 403; the verifier cannot read its keys -> 503 with `Retry-After` (never
an accept). Refused tokens are charged to one shared failure budget
(`daemon.event_ingest.failed_auth_per_second`, default 50, and
`failed_auth_burst`, default 100); when it is spent the listener answers 429
without verifying further, so a flood of garbage bearers cannot burn CPU.
Successful requests are never charged to it. Accepted requests are logged as
`remote service JWT accepted sub=... jti=<8 chars>` and recorded in the identity
audit; the token is never logged.
