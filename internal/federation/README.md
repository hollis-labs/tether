# internal/federation

Authority-routing for Tether's `go-messaging` stack — the seam that lets
Tether participate in federated, cross-app messaging.

**Purpose:** make "internal vs external" a single routing question instead
of a schema fork. Every message URN carries an `<authority>` segment; a
message is foreign precisely when its authority is owned by another daemon.

**Entry points:**

- `Config` / `Peer` (`config.go`) — the `federation:` YAML block embedded
  in `config.Global`. The zero value (`enabled: false`) is a standalone
  install; `Validate` is called from `config.Catalog.Validate`.
- `Router` (`router.go`) — a `messaging.Store` decorator that dispatches
  each operation by the recipient URN authority: local authority → local
  store, registered peer → peer store. `Get`/`Thread`/`Cancel` are
  id-keyed and always local.
- `Dialer` / `HTTPDialer` (`peerstore.go`) — turns a configured `Peer` into
  a `messaging.Store`. `HTTPDialer` speaks a remote daemon's `/messages/*`
  HTTP routes — the cross-host counterpart of `internal/api`'s messages
  surface.
- `BuildRouter` (`federation.go`) — composes a `Config` + the local store
  into a `Router`; returns `nil` when federation is disabled.

**Neighbors:**

- `internal/app` builds the Router at startup and exposes it as
  `Service.Federation` (nil when disabled).
- `internal/store` supplies the local `messaging.Store` the Router wraps.
- `internal/api` / `internal/mcpadapter` serve the `/messages/*` routes a
  peer's `HTTPDialer` store calls.

**Gotchas:**

- Federation is **opt-in**. With `enabled: false` no Router is built and
  messaging is unchanged — do not assume `Service.Federation` is non-nil.
- The peer hop is **plain HTTP**. Cross-host auth (mTLS / signed envelopes)
  is program task M2 (`CW-20260518-0040`); only enable peers inside a
  shared trust domain until then.
- The Router duplicates the decorator promoted into `go-messaging`
  (`CW-20260518-0049`) because Tether pins `go-messaging v0.2.1`, which
  predates that release. The API was kept matching for a future drop-in
  swap.

See [ADR-0040](../../docs/adr/0040-messaging-federation-peer-routing.md)
and [docs/messaging-federation.md](../../docs/messaging-federation.md).
