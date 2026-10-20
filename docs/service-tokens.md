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
token at once; any other verifier stops accepting it within its poll interval
(30 s by default, the library's).

## The deny-list is public (minimal)

`GET /auth/denylist` (and `HEAD`) needs no credential, no scope and no protocol
header, on the local and the remote listener. It returns exactly three arrays of
opaque ids, `jti`, `sub` and `sid`, with an `ETag` and `If-None-Match` support;
never a token, never a label, never a scope. A verifier that cannot read the list
cannot honor a revocation, and nothing in it is secret. **Writing** it
(`POST /auth/denylist`) is unchanged and needs `tether.admin` (or the operator
over the local socket). Earlier, reading it needed `tether.maintain`; a poller that
still sends a bearer is unaffected.

## Scope rules

- Explicit verbs by default. `<audience>.*` is accepted only with
  `--allow-wildcard` (dev), and each use logs a warning.
- For audience `tether`, a service token may hold only `tether.events`,
  `tether.read` and `tether.messages`. Nothing that administers Tether, launches
  sessions or edits the catalog.
- The audience and the scope prefix must match; reserved enrollment audiences are
  refused.
- At most 256 live service tokens; revoke unused ones.

## What it is not (yet)

Reachability from other hosts over the remote listener is a separate change under
CW-20261019-0044. A service token does not yet authenticate on the remote
listener, which accepts only paired devices.
