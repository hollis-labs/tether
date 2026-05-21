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

A standalone install registers no peers, so every authority is local and
messaging behaves exactly as it did before federation existed. Federation
is **off by default** and purely additive.

## Configuration

Add a `federation:` block to `global.yaml`:

```yaml
federation:
  enabled: true
  local_authority: tether        # the URN authority this daemon owns
  strict: false                  # optional; see "Strict mode" below
  peers:
    - authority: torque           # a foreign URN authority
      base_url: http://10.0.0.4:7777
    - authority: nanite
      base_url: http://10.0.0.5:8080
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

`POST /messages/notify` is a local daemon convenience wrapper around the same
message envelope model. It first stores the message, then best-effort injects a
mailbox wake turn into a live session when the recipient resolves locally:

- `msg://session/<authority>/<session_id>` wakes that live session.
- `msg://agent/<authority>/<logical_agent_id>` wakes the newest running local
  session for that logical agent.
- An explicit `session_id` in the notify body overrides recipient resolution.

Unknown or offline recipients still receive durable mail when the send routes
locally; the response reports `wake_attempted`, `wake_delivered`, and
`wake_error`.

Current federation caveat: notify wake injection is local-session behavior.
Cross-host wake requires the recipient authority's daemon to receive a notify
request and resolve its own live sessions. Plain `Send`/`Inbox`/`Subscribe`
federation is the base layer; cross-host notify orchestration belongs with the
M2 hardened transport work.

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
startup. Routing the live MCP/HTTP message-send and inbox paths through it
(rather than the local store directly) is the remaining integration step —
see ADR-0040 "Consequences".
