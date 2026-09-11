# Integrating an application with Tether messaging

For someone **implementing** messaging inside an application — deciding where
identity is minted and stored, what happens across a restart, and how the app
behaves when the daemon is not there. It assumes you have already read
[messaging-adoption.md](./messaging-adoption.md), which is the orientation: the
actor/session/binding model, the identity-parameter rules, and the three ways
in. This document does not repeat them.

Sections are independent. Go to the one matching the decision in front of you.

Tether **v0.6.0** and `go-tether-client` **v0.2.0** or newer. Behavior described
here against earlier versions is not the behavior you will get.

---

## 1. Where identity comes from, and who keeps it

The decision that costs the most to get wrong, because changing it later means
migrating every message already addressed to the old URN.

**The app owns one durable actor per thing that receives mail** — not one per
process, not one per session. If two processes are two deployments of the same
logical agent, they share a URN and take turns holding the binding. If they are
genuinely different correspondents, they are different actors.

Mint it once, with `tether_registry_register` or `POST /registry/agents`. The
server assigns the URN; supplying one is `invalid_request`. **Persist what comes
back in the app's own storage.** Tether's registry is a directory, not your
configuration store — an app that re-registers on every boot because it did not
save the URN creates a new actor each time and orphans all prior mail.

A useful shape: URN in the app's config or DB, written once at provisioning,
read at every boot, never derived. If it is absent, that is a provisioning
error worth failing loudly on, not a cue to register again.

### Idempotency at the launch boundary

`ResolveSessionBootstrap` (Go) and `POST /sessions/bootstrap` are the
launch-boundary helper. They are idempotent: a repeated call for the same
preassigned session id returns the same identity rather than minting a
competing one.

They are also deliberately **offline-safe**, which shapes your error handling:

```go
res, err := tether.ResolveSessionBootstrap(ctx, opts)
// res.SessionID   — always usable, even with no daemon at all
// res.Registered  — whether the daemon accepted the registration
// res.RegisterErr — why not, if not
```

`err` being nil does **not** mean the daemon saw you. Check `res.Registered`
separately. Treating a successful local resolution as proof of registration is
the mistake this split exists to prevent.

---

## 2. Receiving: pull, and when push is available

Two models, and the choice is not free.

**Pull** — on a cadence you choose. Works everywhere, with no binding and no
daemon-hosted session. **Two different calls, and the distinction matters:**

- `mux_message_list` / the client's list read is **non-destructive and
  repeatable**. This is the polling call.
- `mux_message_inbox` / `Inbox` is an **atomic-delivery pull**: returned
  messages are marked delivered and **will not come back on a later inbox
  call**. It is a take, not a look.

Build the poll loop on `list`. Reach for `inbox` only where exactly-once
hand-off to one consumer is what you want, and where losing the batch on a
crash between the pull and the work is acceptable.

**Push** — the daemon wakes a live session when mail arrives. Requires a
**runtime binding** leased against a session *the daemon is hosting*. An app
that launches its own processes outside Tether cannot use this, and the honest
design is to build pull and treat push as an optimization available in the
hosted case.

Leasing needs `target_urn`, `session_id`, `host_id`, `attempt_id`, and
`capabilities` exactly `["pull-only"]` — the lease endpoint accepts nothing
else, because caller-supplied-webhook push bridging is not implemented; the
bridge pulls its own mailbox. Leasing against a target already bound to a
Tether-managed session is refused rather than superseding it.

### Binding lifecycle across a restart

A lease has a TTL. `renew` extends it and fails `conflict` if a newer
generation now exists for the same target — which is exactly what a crashed
predecessor's replacement looks like, so treat `conflict` on renew as "someone
else legitimately took over," not as an error to retry.

`revoke` on clean shutdown is good hygiene. A crash skips it, and the lease
expires on its own — mail accumulates against the actor in the meantime and the
next binder receives it. Nothing is lost by not revoking; you just leave a
window where the daemon thinks a dead session is current.

---

## 3. Reading is not handling

`list` does not clear anything, and `consume` is the call that records a
message as handled. The two are deliberately separate so a crash between
reading and acting does not lose the message.

Note `inbox` sits between them and is easy to misread as a browse: it does not
mark a message *consumed*, but it does mark it *delivered*, which is enough to
keep it out of every later inbox call. Pulling with `inbox` and crashing before
you act loses the batch from that view — which is exactly the failure this
section is about.

**Consume after you have acted, not after you have read.** An app that consumes
on read and then crashes has silently dropped work, and nothing in the system
will tell you. Delivery is at-least-once against the recipient's behavior, not
exactly-once against your intentions.

`mark_read`, `archive` and `unarchive` are presentation state for a human-facing
inbox. They are not delivery state and do not affect redelivery. Do not build
"have I processed this" on top of them.

---

## 4. Errors an app has to handle by name

Every non-2xx carries `{"error": {"code", "message"}}` (ADR 0010). The three
that will actually reach your messaging code:

| Code | HTTP | What it means here, and what to do |
|---|---|---|
| `invalid_request` | 400 | Most often a missing identity parameter. Note the name varies by tool — see the adoption guide's table. A bug in your call, not a transient fault; do not retry. |
| `forbidden` | 403 | `as` did not match the message's sender or recipient. Also a bug: you asked for someone else's mail. Do not retry, and do not fall back to omitting `as`. |
| `conflict` | 409 | A state precondition failed — on `renew`, a newer generation exists. Usually legitimate, and the right response is to stop, not to retry harder. |

`locked` (423) appears on group writes to an archived room. `internal_error`
(500) is the only one where a bounded retry is reasonable.

The failure mode worth naming: **treating every non-2xx as transient and
retrying.** Three of the four above are permanent for the request as written, so
a blanket retry turns a visible bug into a silent loop.

---

## 4a. Response casing is not uniform

Request bodies are `snake_case` throughout. Responses are not.

`/whoami`, `/messages/*` and the registry profile routes respond in
`snake_case` — those structs carry JSON tags. The **binding** routes marshal
`registry.RuntimeBinding` and `registry.ScopedBinding` directly, and those
types have no tags, so they come back **`PascalCase`**:

```json
{ "ID": "bnd_1", "TargetURN": "msg://agent/a/b", "Generation": 3,
  "Capabilities": ["pull-only"], "Visibility": "published-local",
  "LeaseExpiresAt": "2026-09-11T15:42:23Z" }
```

If you are in Go, decode into the `registry` types and this never comes up. If
you are not, do not write one snake_case decoder and point it at every route —
lease, renew, list and the scoped-binding routes will all silently produce
zero values.

---

## 5. What routes through the daemon, and what must not

Session-mutating operations go through the daemon (ADR 0035). An app that
reaches around it and mutates session state directly splits brain with the
daemon's live runtime handles — the daemon keeps serving from handles that no
longer describe reality, and the divergence surfaces later as delivery to a
session that is gone.

Concretely: do not open Tether's state database and write to it. Read-only
inspection for debugging is fine. Anything that changes state goes over HTTP or
a typed client.

The same boundary applies to the registry: it holds identity and a callback
URI, never operational content. Do not put config, tokens, or `env` into a
profile — ADR 0041 D18 is explicit, and substrate catalog YAMLs carry plaintext
OAuth tokens, which is why the registry has no payload cache to copy them into.

---

## 6. Testing against it

`go-messaging` ships `messagingtest`, a conformance suite that runs against any
`messaging.Store` implementation. If your app implements the `Store` contract,
run it — `go-tether-client` does, and that suite is what established its
v0.5.1 upgrade was source-compatible.

For app-level integration, prefer a real daemon over a mock. Tether's own
`e2e/` package launches real processes and includes an upgrade drill against a
pre-migrations fixture; a mock that agrees with your assumptions will keep
agreeing with them after they stop being true.

Do not assert on message *ordering* across different senders. Nothing promises
it.

---

## 7. Before you go to production

Migrations apply idempotently on daemon boot. The v0.6.0 set (`0019`–`0022`) is
additive.

**Rollback is not symmetric.** Running new-schema data against an older binary
is unsupported — older code does not know the new tables exist. If you need a
rollback path, snapshot the database before the first boot on the new binary and
restore that alongside the old one. Additive migrations make the forward step
safe, not the backward one.

---

## What this does not cover

- **Federation across hosts** — [messaging-federation.md](./messaging-federation.md).
- **A2A interoperability** — inbound relay only; Tether is not an A2A client
  (CW-20260907-0028). T12's handoff §3.7 has the binding format.
- **Per-tool parameter reference** — [mcp.md](./mcp.md) carries a table per
  tool, and [api/README.md](./api/README.md) an entry per route. `mux mcp`
  remains authoritative if the two ever disagree.
- **Durable claim/ack/nack** — the primitives exist as raw HTTP routes but have
  no typed client wrapper (CW-20260907-0038). If your design depends on them,
  that gap is the first thing to close.
- **Operating the daemon** — install, catalog, sandboxing all live elsewhere in
  `docs/`.
