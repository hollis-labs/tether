# Messaging Federation

Tether participates in **federated, cross-app messaging** via
authority-routing. This is the operator-facing guide; the design rationale
is [ADR-0040](adr/0040-messaging-federation-peer-routing.md), and the code
is [`internal/federation`](../internal/federation).

## The idea

Every message carries a URN address — `msg://<kind>/<authority>/<id>`. The
`<authority>` segment is the email-style "domain" that owns the addressed
entity. Federation is one question, not a subsystem: **is a message's
authority local or foreign?**

- Local authority (or any authority with no peer) → served by Tether's own
  SQLite messaging store.
- A registered peer authority → routed to that peer's daemon over HTTP.

A standalone install registers no peers, so every authority is local.
Recipient wake described below also applies to locally delivered mail.
Federation is **off by default**.

## Configuration

Add a `federation:` block to `global.yaml`:

```yaml
federation:
  enabled: true
  local_authority: tether        # the URN authority this daemon owns
  strict: false                  # optional; see "Strict mode" below
  peers:
    - authority: torque           # a foreign URN authority
      base_url: http://192.0.2.4:7777
    - authority: nanite
      base_url: http://192.0.2.5:8080
```

- **`enabled`** — when `false` (the default) the whole block is ignored and
  the daemon runs as a standalone install.
- **`local_authority`** — required when enabled. A single URN segment (no
  slash, no whitespace). Messages to this authority — and to any authority
  without a peer — are served locally.
- **`peers`** — each binds a foreign `authority` to the `base_url` of the
  daemon that owns it. The `base_url` is the root of that daemon's
  `go-messaging` HTTP surface; the `/messages/*` routes are resolved against
  it. Peer authorities must be unique and must not equal `local_authority`.
- **`strict`** — see below.

An invalid block fails catalog validation at daemon startup.

## What routes where

The router dispatches on the **recipient** authority:

| Operation              | Routed by            |
|------------------------|----------------------|
| `Send`                 | `env.To.Authority`   |
| `Inbox`, `Subscribe`   | recipient authority  |
| `Consume`              | recipient authority  |
| `Get`, `Thread`, `Cancel` | always local — these take an opaque id with no authority |

Cross-authority `Get`/`Cancel` is intentionally out of scope: it needs the
cross-host transport from program task M2.

## Notify and wake

`POST /messages/notify` stores the same message envelope and retains its local
best-effort wake behavior. Federated `Send` now carries recipient wake intent
to the receiving daemon, which evaluates its own live recipient:

- `msg://session/<authority>/<session_id>` wakes that live session.
- `msg://agent/<authority>/<logical_agent_id>` follows the current runtime
  binding and generation fence. Legacy running-session fallback applies only
  where the existing binding policy permits it.
- An explicit local notify `session_id` must still match the recipient.

Unknown or offline recipients still receive durable mail when the send routes
locally; the response reports `wake_attempted`, `wake_delivered`, and
`wake_error`.

The additive envelope metadata `tether.wake_intent` contains JSON with
`wake_text`, `urgency`, and `no_wake`. Recipient policy wins: pull-only/external
bindings never wake, stopped sessions are never launched or resumed, and
binding-generation checks remain in force. `no_wake` also fences the existing
retry pump. Missing intent uses the receiving daemon's ordinary mailbox wake.
Mail storage succeeds independently of wake; response-only
`tether.wake_outcome` reports attempted/delivered/reason/detail.
Generated wake notifications use the accepted intent's urgency, falling back
to legacy envelope urgency and then `normal` when no intent urgency exists.

`tether.message_id` is the stable source message identity. The existing shared
delivery core scopes it by sender and rejects a changed immutable envelope or
intent. The recipient keeps one canonical message row. Reuse the same source
identity when retrying a request; a fresh identity means a fresh message. The
HTTP peer adapter uses a supplied identity, otherwise the envelope ID, otherwise
one newly minted identity for that send operation. It does not promise retry
identity for a caller that discards a failed request's identity.

Wake admission is durable and separate from mail. Ordinary redelivery does not
submit another wake. A crash after admission but before recording its outcome
can mean either before or after provider submission; it is reported as
`wake-outcome-unknown` and is not guessed into another turn. A recorded busy or
failed attempt remains eligible for the existing fenced retry pump. A confirmed
successful outcome suppresses a resend after consume-lease expiry. A crash
during a retry, after provider submission but before updating that outcome,
can leave the earlier failure recorded; the existing lease may then permit
another retry. Admission, provider submission and outcome persistence are not
one atomic operation. Mail remains available even when wake is refused or
uncertain. This is not crash-proof exactly-once provider execution. Cross-host sender
authentication/reply authority remain separate future work; current source
acceptance uses two isolated local daemons only.

## Strict mode

With `strict: false` (default) a message to an unknown authority falls
through to the local store — a misconfigured peer never hard-fails local
traffic. With `strict: true` an unknown authority is rejected with
`ErrNoRoute`. Turn it on when a misaddressed envelope should be a loud error.

## Security / transport

The peer hop currently uses **plain HTTP** over the remote daemon's
`/messages/*` routes. Cross-host authentication — mTLS, signed envelopes —
is program task M2 (`CW-20260518-0040`). Until that lands, only enable
peers within a shared trust domain: loopback, a private network, or a
tunnel. The transport is pluggable (`federation.Dialer`) and accepts a
custom `*http.Client`, so a hardened transport slots in without code
changes elsewhere.

## Status

The `federation.Router` is composed onto `app.Service.Federation` at daemon
startup. The daemon already wraps its message store with that router, forwarding
Send/Inbox/Subscribe/Consume by recipient authority. The recipient's canonical
local store now performs the post-commit wake decision. This source description
does not claim any production peer configuration or live cross-host acceptance.
