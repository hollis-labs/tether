# ADR 0007: Event Bus — Drop-Oldest + Slow-Consumer Eviction

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-06 (Eventing), task T-v002-s06-01
**Deciders:** agent-mux v0.0.2 execution session

## Context

The event bus multiplexes lifecycle events from three producers (runtime
session transitions, daemon lifecycle, broker envelope writes) to N
subscribers (the SSE `/events/stream` endpoint, future Nanite/Clockwork
consumers). Bus writes run on critical paths — a session-state
transition emits an event *synchronously* from the runtime manager.
If the bus blocked on a slow subscriber, the manager would block, which
would back-pressure through to the PTY loop.

Three drop strategies were considered:

1. **Block-and-wait** with a per-subscriber queue. Pros: zero loss.
   Cons: one stuck subscriber halts the whole daemon.
2. **Drop-newest** when the subscriber queue is full. Pros: simple.
   Cons: subscribers keep seeing stale history and miss the interesting
   recent event they need for debugging.
3. **Drop-oldest** when the subscriber queue is full. Pros: recent
   events win (typically more informative). Cons: subscribers lose
   mid-stream history; the consumer contract must tolerate gaps.

## Decision

**Drop-oldest with a consecutive-drops threshold.**

- Each subscriber has a bounded channel (default depth configurable via
  `BusOptions.SubscriberDepth`).
- When the channel is full, the bus drops the oldest queued event for
  *that subscriber* (other subscribers are unaffected).
- A per-subscriber `MaxConsecDrops` counter increments on each drop.
  When the threshold is hit, the subscriber is evicted: channel closed,
  deregistered, cancel func invoked. Prevents chronically-slow
  subscribers from pinning memory or delaying shutdown.
- `SinceSeq=0` in a `Filter` means "full replay", not "empty replay".
  Callers that only want live events pass a very large `SinceSeq`.
- Replay-then-live ordering is guaranteed via an internal `done`
  channel. The bus never closes the live channel while producing — that
  pattern trips `-race` in the way we confirmed during Sprint 6.
- Persistence is **synchronous before notification.** An event is
  inserted into the store, the resulting row id becomes `Seq`, then
  subscribers are notified. A persist failure does not publish.

## Alternatives Considered

- **Block-and-wait:** rejected (stuck subscriber halts daemon).
- **Drop-newest:** rejected (stale history hides the signal most
  subscribers care about).
- **Unbounded per-subscriber buffer:** rejected (memory pressure scales
  with session lifetime; pathological slow consumers can OOM the
  daemon).
- **Emit-then-persist:** rejected (a persistence failure should not
  leave subscribers with an event that no historical replay will show).

## Consequences

- Subscribers see a best-effort live stream. Gaps are possible when the
  subscriber is slow; re-subscribing with `since_seq=<last_seen>`
  recovers missed persisted events.
- The runtime manager never blocks on event publishing. Session-state
  transitions remain fast regardless of subscriber health.
- `MaxConsecDrops` eviction is a safety net, not the norm. A healthy
  subscriber never hits it.
- This policy is load-bearing for the SSE `/events/stream` endpoint —
  the endpoint's HTTP handler is a subscriber itself and relies on
  drop-oldest to avoid blocking the HTTP response writer.
- `session.state_changed` payloads are small JSON blobs; bus memory
  per event is bounded. Large payloads would stress drop-oldest less
  than block-and-wait, but we keep payloads small as a convention.
