# ADR 0040: Messaging Federation — Authority-Routing Peer Configuration

**Status:** Accepted — 2026-05-18
**Context:** CW-20260518-0051 (Torque Messaging program, plan CW-20260518-0038, phase ph-6)
**Extends:** ADR-0023 (Message Routing Contract)

## Context

Tether already speaks `go-messaging`: every message is an `Envelope` with a
typed URN `Address` (`msg://<kind>/<authority>/<id>`), and Tether's SQLite
`messagingStore` is a contract-conformant `messaging.Store`. What Tether
lacked was a way to participate in **federated, cross-app messaging** — to
deliver an envelope to a daemon other than itself.

The Torque Messaging design study (`docs/torque-messaging-design.md`)
settled the approach: federation is **authority-based routing, never a
schema fork**. The URN `Authority` segment is the email-style "domain". A
message is "external" precisely when its authority is owned by another
daemon. This makes "internal vs external" one routing question instead of a
parallel schema.

## Decision

### 1. An authority-routing `Store` decorator

`internal/federation.Router` decorates a `messaging.Store`. It holds one
local store plus a registry of foreign-authority peer stores and dispatches
each operation by the **recipient** authority:

- `Send` → `env.To.Authority`
- `Inbox`, `Subscribe` → `to.Authority`
- `Consume` → `recipient.Authority`

`Get`, `Thread`, and `Cancel` are keyed by an opaque envelope/thread id,
carry no authority, and are served from the local store. Resolving a
foreign envelope id requires the cross-host transport of program task M2
and is out of scope here.

### 2. Standalone is the zero-config default

Federation is **opt-in and purely additive**. The `federation:` config
block's zero value (`enabled: false`) is a standalone install: no Router is
built, and messaging behaves exactly as it did before this ADR. An
`enabled` install with no peers simply declares a local authority. Only an
`enabled` install with `peers:` routes anything cross-host.

### 3. Peer configuration

The `federation` block in `global.yaml` declares the local authority and a
list of peers (`{authority, base_url}`). `federation.BuildRouter` validates
the block, constructs the Router, and registers each peer. The daemon wires
the resulting Router onto `app.Service.Federation`.

### 4. The peer hop is HTTP over the existing `/messages/*` surface

A peer store (`HTTPDialer`) is a `messaging.Store` implemented over a remote
daemon's `go-messaging`-native `/messages/*` routes — the same routes
Tether's own `internal/api` serves. The transport is pluggable via the
`Dialer` seam and accepts a caller-supplied `*http.Client`.

### 5. `Strict` mode is off by default

With `strict: true` the Router rejects an authority that is neither local
nor a registered peer (`ErrNoRoute`) instead of falling through to the local
store. The default (lenient fall-through) keeps a misconfigured peer from
hard-failing local traffic.

## Consequences

- Tether can route envelopes to peer daemons; a standalone install is
  unaffected and unconfigured.
- **Out of scope / deferred:** cross-host transport authentication (mTLS,
  signed envelopes) is program task M2 (CW-20260518-0040). `HTTPDialer`
  ships a plain-HTTP transport for a shared trust domain (loopback, private
  network, or tunnel); a hardened `*http.Client` slots in without touching
  the Router.
- **Follow-up:** the Router is composed onto `Service.Federation` but the
  MCP/HTTP message front-ends still call `Store.MessagingStore()` directly.
  Cutting those send/inbox paths over to `Service.Federation` is the
  remaining integration step (mirrors the Nanite adoption's deferred
  front-end cutover, CW-20260518-0050).
- The Router intentionally duplicates the decorator promoted into
  `go-messaging` (CW-20260518-0049). Tether pins `go-messaging v0.2.1`,
  which predates that release; swapping to the library `Router` is a
  drop-in once `go-messaging` is retagged — the API was kept matching.
