# ADR 0002: Daemon Transport

**Status:** Accepted — 2026-04-18
**Context:** Sprint v002-01 (Daemon Runtime), task T-v002-s01-02
**Deciders:** agent-mux v0.0.2 execution session

## Context

v0.0.2 introduces a long-lived daemon (`muxd`) that owns running sessions
across CLI invocations. The CLI (Sprint v002-s01-03) and the future local
API (Sprint v002-s05) both need a transport to talk to the daemon.

Three candidates were considered:

1. **Unix domain socket only** — POSIX-only, filesystem permissions act as
   ACL, no port collisions, no network stack.
2. **TCP on loopback only** — cross-platform (Windows-ready), `curl`-friendly,
   familiar debugging workflow, needs port management.
3. **Both, concurrent** — two listeners serving the same router, hedges bet.

Sprint readiness notes explicitly accept a Unix-only target for v0.0.2
(Windows support is deferred), and the sprint "Fix direction" says
"scaffold for both."

## Decision

Single config field `daemon.listen_addr` with a scheme prefix:

- `unix:/path/to/sock` → `net.Listen("unix", path)`
- `tcp:host:port` → `net.Listen("tcp", host:port)`

A thin `daemon.Listener(addr string) (net.Listener, error)` helper parses
the scheme and returns `net.Listener`. The same HTTP router
(`http.Serve(listener, handler)`) is used regardless of transport, so
adding a second listener later is trivial (just call `Listener` twice and
spawn two `http.Serve` goroutines).

**Default:** `unix:~/.agent-mux/run/muxd.sock`

Rationale for the default:

- Filesystem permissions give a natural single-user ACL; no accidental
  multi-user exposure on a shared host.
- No port-collision class of problems (e.g., with other tools that grab
  7100–7200).
- TCP remains a one-line config change (`listen_addr: tcp:127.0.0.1:7180`)
  when debugging ergonomics demand it (`curl`, Postman, browser devtools).

## Consequences

**Positive**

- One code path, two backends. Adding the second listener concurrently
  (e.g., unix + tcp for dev inspection) is a small follow-up, not a
  re-architecture.
- Unix socket default means the daemon can't be reached from another user
  account on the same machine without explicit socket-path access.
- No dependency on a port-management layer in v0.0.2.

**Negative**

- Windows support is not in v0.0.2. When it arrives, the default must
  change (or be platform-conditioned) because Unix sockets have uneven
  Windows behavior. Record as a follow-up on the epic.
- `curl` against a Unix socket requires `--unix-socket` or a TCP override.
  Documented in the daemon README when it lands.

## Notes for future ADRs

- If the API surface grows (Sprint v002-s05) and we want to expose it on
  both unix and tcp simultaneously, this ADR does not need to be
  re-litigated — extend `daemon.Config` to accept a slice of
  `listen_addr` entries.
- If mTLS or token auth becomes a requirement, that is a separate ADR
  (auth model is out of scope here).
