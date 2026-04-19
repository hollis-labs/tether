# ADR 0011: Local API — Attach Stays `octet-stream`, SSE Reserved for Events

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-05 (Local API), tasks T-v002-s05-02 and T-v002-s05-05
**Deciders:** agent-mux v0.0.2 execution session

## Context

Two streaming endpoints landed in Sprint v002-05:

1. `GET /sessions/{id}/attach[?since_seq=N]` — live PTY bytes from a
   running session. Bytes are opaque (terminal control sequences, ANSI
   colors, raw UTF-8 — whatever the child writes).
2. `GET /events/stream[?scope=…&since_seq=…]` — structured lifecycle
   events (session state changes, daemon lifecycle, broker envelopes).
   Events are JSON objects with a known schema.

The question: should both use the same transport, and if so, which?

## Decision

**Two different transports, each fit-for-purpose:**

- **Attach** uses `Content-Type: application/octet-stream` with a
  flushed HTTP response body. Raw PTY bytes stream byte-for-byte into
  the client. `since_seq=<int>` in the query string resumes from a
  byte offset in the broker's ring.
- **Events** uses Server-Sent Events (`Content-Type: text/event-stream`):

  ```
  id: <seq>
  event: <kind>
  data: {"scope":"session","session_id":"…","payload_json":"…"}

  ```

  Keep-alive via periodic `: ping` comments every 15 s. Filters:
  `?since_seq=`, `?scope=` (repeatable), `?session_id=`.

## Alternatives Considered

- **Both via SSE.** Rejected: SSE frames wrap each chunk in
  `data: <…>\n\n`. Terminal byte streams frequently contain characters
  that require escaping in SSE (CR, LF); either the server re-frames
  every chunk (double the CPU + every ANSI sequence breaks framing) or
  clients accept broken output. Terminal emulators want raw bytes.
- **Both via octet-stream.** Rejected: event consumers want `id:` +
  `event:` fields so they can dedupe across reconnects and branch by
  kind without parsing every frame.
- **WebSocket for both.** Rejected: WebSocket is bidirectional, which
  neither endpoint needs. Input is `POST /sessions/{id}/input`; events
  are read-only. Adding WS would duplicate transport state.
- **gRPC streams.** Rejected: requires a protobuf compiler in the
  build, adds a binary-framing negotiation hop, and forecloses
  `curl`-level debugging — which was a Sprint 5 readiness requirement.

## Consequences

- Attach clients are trivially `curl --unix-socket … /sessions/<id>/attach`.
  Terminal emulators and simple scripts work without a parser.
- Event clients use standard SSE libraries or hand-write a tiny parser
  — `id:`/`event:`/`data:` blocks separated by blank lines.
- A single endpoint that "just streams everything" does not exist.
  Clients that want both subscribe to events *and* attach.
- Resume semantics for both use `since_seq`, but the seq spaces are
  different (attach: broker byte offset; events: table row id). Don't
  mix them.
- Related ADRs: [0009](0009-local-api-create-launch-split.md),
  [0010](0010-local-api-typed-error-envelope.md).
- Full endpoint reference: [`docs/api/README.md`](../api/README.md).
