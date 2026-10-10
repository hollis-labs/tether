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
    - authority: worker           # the recipient environment authority
      base_url: http://127.0.0.1:17331 # existing SSH local forward
      credential_ref: file:///home/operator/.tether/credentials/worker.token
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
- **`credential_ref`** — an optional explicit `file:///absolute/path` to the
  device token issued by the recipient. The file must be regular, owned by the
  daemon user, and mode `0600`; a final symlink or special file is refused. Only
  the reference belongs in YAML. No environment, helper or default operator-token
  lookup is performed. Authenticated peers require an HTTP loopback endpoint.
- **`strict`** — see below.

An invalid block fails catalog validation at daemon startup. Credential file
contents are read on each request, so replacing the private file updates later
requests without restarting the daemon. A missing, insecure or invalid file
fails that peer request; it never falls back to anonymous delivery and does not
prevent local mail from working.

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

Authenticated federation uses a device bearer on the recipient's always-enforce
remote listener. Point `base_url` at an already established SSH local forward
reaching that listener; Tether does not create or repair the tunnel. Use a
separate peer-issued credential for each direction. The local operator token is
not a federation credential and the remote listener rejects it.

The recipient requires `operate` for Send/Consume and `read` for Inbox/Subscribe.
These are independent permissions; no child execution or native-resume authority
is implied. Inbox and Subscribe send the canonical mailbox argument as `as=to`.
Envelope `from` and mailbox `as` remain caller assertions; authenticating the
transport does not establish verified actor attribution. Strict routing and
unknown-authority behavior remain as described above.

The client preserves context-owned SSE lifetime and the supplied HTTP client's
timeout and redirect settings. Authenticated peers require a standard HTTP
transport; arbitrary transports are refused. The client clones the transport,
disables proxies and checks the actual connection's loopback address and peer
port before sending headers, including when a custom dialer or `localhost` is
used. It clones requests and refuses redirects to another origin before sending
either mail or credentials. File resolution errors are identifiable as
`ErrPeerCredential`; HTTP401 (bad, expired or revoked token) as
`ErrPeerAuthentication`; HTTP403 as `ErrPeerScope`. `PeerAuthError` includes the
peer authority and HTTP status when available, without credential bytes or peer
response-body text. Revocation also ends authenticated recipient streams under
the existing device watcher; it does not stop agents or change their custody.

Omitting `credential_ref` preserves the legacy unauthenticated peer transport;
it cannot access a protected remote listener. mTLS, signed envelopes and verified
sender/reply authority remain separate work. The same-user credential-file and
token-database access limitation remains accepted for this MVP. Source tests use
disposable credentials and two local loopback daemon HTTP compositions; they do
not establish live worker enrollment, SSH carrier readiness or deployment.

## Status

The `federation.Router` is composed onto `app.Service.Federation` at daemon
startup. The daemon already wraps its message store with that router, forwarding
Send/Inbox/Subscribe/Consume by recipient authority. The recipient's canonical
local store now performs the post-commit wake decision. This source description
does not claim any production peer configuration or live cross-host acceptance.
