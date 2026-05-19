// Package federation gives Tether's messaging stack authority-routing:
// the ability to participate in federated, cross-app messaging without
// forking the envelope schema.
//
// Tether already speaks go-messaging — every message carries a typed URN
// Address whose Authority segment is the email-style "domain" that owns
// the addressed entity. Federation is therefore one question, not a new
// subsystem: is a message's authority local or foreign? This package
// answers it with a Router, a messaging.Store decorator that dispatches
// each operation by the recipient's authority:
//
//   - the local authority (and any authority with no peer) → the local
//     store — Tether's own SQLite messaging store;
//   - a registered peer authority → that peer's store — an HTTP-backed
//     store reaching the daemon that owns the authority.
//
// A standalone Tether install registers no peers, so every authority
// resolves locally and messaging behaves exactly as it did before
// federation existed. Federation is purely additive and disabled by
// default: see Config, whose zero value is a standalone install.
//
// Layering:
//
//   - Config / Peer       — the `federation:` YAML block (config.Global).
//   - Router              — the authority-routing messaging.Store decorator.
//   - Dialer / HTTPDialer — turns a configured Peer into a peer store.
//   - BuildRouter         — composes a Config + local store into a Router.
//
// Cross-host transport authentication (mTLS / signed envelopes) is the
// concern of program task M2 (CW-20260518-0040); HTTPDialer ships a plain
// HTTP transport suitable for a shared trust domain (loopback, a private
// network, or a tunnel) and accepts a caller-supplied *http.Client so a
// hardened transport can be slotted in without touching this package.
package federation
