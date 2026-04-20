# ADR 0017 — Claudestream Event Routing

**Status:** accepted
**Date:** 2026-04-19
**Supersedes:** —
**Superseded by:** —

## Context

Sprint v004-02 introduces a new provider (`claude-stream`) that runs `claude` CLI as a subprocess with `--output-format stream-json` and parses its output into structured events via `pkg/claudestream` — text deltas, tool_use blocks, usage reports, session_id for `--resume`. Unlike PTY-backed providers that stream raw bytes, claudestream produces a structured NDJSON event stream.

Question: how should these events reach client consumers (TUI chat surface, future MCP server, future headless clients)?

Three options were considered:

- **(a) Extend `provider.Session` with `Events() <-chan Event`.** Every provider implements it; PTY-based providers return a closed empty channel.
- **(b) New `EventfulSession` interface.** Subtype of `Session`; runtime type-asserts on demand.
- **(c) Events flow through existing `events.Bus`.** A new event kind carries claudestream events; subscribers filter by session_id + kind. Symmetric with v0.0.2's SessionStateChanged pattern.
- **(d) Events flow through the existing attach stream as NDJSON.** No contract changes — `Session.Fanout` writer already carries session output; we just let claudestream's JSON lines be that "output." Attach clients consume the stream and parse events themselves.

## Decision

**(d) — Events ride the existing attach stream as NDJSON.**

The claudestream session's goroutine writes each JSON event line directly to `StartOptions.Fanout` (same writer the PTY adapter uses for PTY bytes). The daemon's existing attach broker fans those bytes to all subscribers. Clients that care about structured events (TUI chat surface) wrap the attach stream in `claudestream.Scanner` to parse back into typed events. Clients that just want bytes (log tail, debug tooling) see the raw NDJSON.

## Why (d) over the alternatives

- **(a) was the cleanest-looking "Go idiomatic" option but pays a tax every session type doesn't use.** Most sessions aren't event-producing. Adding `Events()` to the base interface forces every implementation to deal with a concept it doesn't need.
- **(b) gets the tax down but adds a second interface to remember + type-assert on.** Subtle bugs when the runtime manager forgets to check for the interface.
- **(c) is symmetric with v0.0.2's event-bus pattern but requires a new `events.ProviderEvent` kind (or a typed-per-provider explosion), plus a new daemon endpoint to expose these events to clients.** Two routes for the same conceptual thing (session output).
- **(d) pays nothing.** Attach is already the canonical "session output" channel, already has multi-client fanout, already has history replay via the ring buffer, already speaks octet-stream (ADR 0011). Claudestream's NDJSON is just another kind of "output." The only adapter the consumer needs is a line-delimited parser — which `pkg/claudestream.Scanner` provides.

## Consequences

- No new daemon API surface. `GET /sessions/{id}/attach` is the single read channel for both PTY bytes and claudestream events.
- The TUI's attach-panel code path and its chat-surface code path share the same `client.AttachStream` call. The difference is what they do with the bytes: attach-panel dumps to a viewport; chat surface pipes through a `claudestream.Scanner` and renders typed events.
- Provider kind is the dispatch key. When the TUI launches a session, it reads the provider ID from the response and picks the right client-side surface:
  - `claude-code` (PTY) → AttachScreen (byte viewport)
  - `claude-stream` (NDJSON) → ChatScreen (typed-event renderer)
- History replay works for both. An attach-reattach cycle gets the same bytes the fanout ring holds — for claudestream that means the prior turn's JSON events replay verbatim.
- `session.log` on disk also receives the NDJSON stream verbatim, so `mux sessions tail` produces a human-legible-ish log (one JSON object per line, greppable by `jq`).

## Non-consequences

- The `events.Bus` stays focused on session lifecycle events (state changes, attach count deltas). Provider-specific payloads don't pollute it.
- The `provider.Session` interface stays at its current shape. ADR 0006 (provider contract stability) holds.
- `GET /sessions/{id}/events` remains for structured lifecycle events, unaffected.

## Alternatives considered more carefully

**Two-channel attach** (bytes on `/attach`, events on `/events`): rejected. Double the bookkeeping per provider, and the distinction is arbitrary — one provider's bytes are another provider's events. Unifying through attach matches the v0.0.2 invariant that "one session has one output stream."

**Server-side event parsing** (daemon parses claudestream into typed events, re-emits via a typed API): rejected for v0.0.4. Adds daemon-side complexity that every new structured-output provider would need a server-side parser shim for. The pkg/claudestream parser is cheap; let each client parse as needed. If server-side parsing becomes valuable later (e.g., for cross-client query/filter), it can sit on top of the current bytes-through-attach model without breaking it.

## Implementation landmarks

- `internal/provider/cli/claudestream/adapter.go` — session goroutine writes to `opts.Fanout` per line.
- `pkg/claudestream/scanner.go` — consumer-side line scanner.
- `internal/tui/detail/chat.go` (Sprint 2 T-04) — wraps `client.AttachStream` → `claudestream.Scanner` → native event rendering.
- `internal/tui/main_screen.go` (Sprint 2 T-04) — dispatches on provider kind after launch to push the right screen (AttachScreen vs ChatScreen).

## Follow-ups

- **Sprint 2 T-04:** chat surface. Validates the contract end-to-end.
- **Sprint 2 T-05:** persist session_id per logical agent. Observed from the first `KindSessionID` event in the attach stream (either client-side in the TUI or daemon-side via a side-channel tap — decide during T-05 readiness).
- **Sandbox integration** (Sprint 3): the claudestream adapter needs to honor `StartOptions.Sandbox` when that field lands, same as PTY providers.
- **When a second structured-output provider arrives** (gemini CLI, codex CLI): re-evaluate whether the Scanner abstraction wants to generalize beyond claude's specific JSON schema. Until then, the pkg/claudestream package stays claude-specific.
