# ADR 0004: Attach Broker — Fan-Out in Runtime.Manager, Drop-Oldest Semantics

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-02 (Live Attach + Input), task T-v002-s02-01
**Deciders:** agent-mux v0.0.2 execution session

## Context

v0.0.2 needed live-attach from multiple concurrent clients while the
PTY producer kept flowing. Three shapes were possible:

1. **Attach lives on `provider.Session`.** Each Session exposes
   `Attach(ctx, w)`. The PTY reader calls into Session-internal state
   to fan out. Pros: single owner of output. Cons: every concrete
   provider (CLI, API, future vendors) reimplements fan-out. CLI and
   API paths diverge. Slow subscriber blocks the producer.

2. **Separate top-level attach service.** A standalone package that the
   daemon wires up. Pros: decoupled. Cons: duplicates the runtime
   manager's registry lookup + adds a second mutex domain; the hop
   between Manager and attach state makes lifecycle reasoning harder.

3. **Attach broker lives inside `runtime.Manager` as a per-session
   struct.** The Session writes to `StartOptions.Fanout` (an
   `io.Writer`); the Manager's `attachBroker` is that writer. The
   broker multiplexes to N subscribers with a bounded per-session ring.

## Decision

**Option 3.** `attachBroker` is owned by Manager, keyed by session ID,
constructed in `Manager.Start` and closed on terminal state.

Semantics:

- Ring buffer of `defaultRingBytes` (64 KiB) per session — enough recent
  history for new subscribers to replay without meaningful memory cost.
- Per-subscriber channel of `defaultSubscriberDepth` (64 chunks).
- **Drop-oldest on slow consumer.** If a subscriber's channel is full,
  the chunk is dropped *for that subscriber only*. The producer never
  blocks. Dropped bytes are counted per broker.
- `close` is idempotent. After close, `Write` returns `io.ErrClosedPipe`
  and new subscribers get the current replay + a closed channel (so
  they can drain history, then exit cleanly).
- All subscriber channels close exactly once — either on individual
  `cancel()` or on broker `close()`, whichever comes first.
- `since_seq` resume (Sprint v002-05 T-02) is computed by
  `computeReplay(ring, totalWritten, sinceSeq)` — a pure function over
  broker state.

## Alternatives Considered

- **Blocking producer on slow consumer:** rejected — a single stuck
  attach would stall the PTY reader and back-pressure through to the
  child process.
- **Unbounded replay:** rejected — `-race` and memory impact scale with
  session lifetime. 64 KiB is a pragmatic middle ground; tunable later.
- **Separate attach server:** rejected for the reasons above.

## Consequences

- CLI and API runtimes share identical attach behavior via the same
  broker code path.
- New runtimes get attach "for free" by writing to `StartOptions.Fanout`.
- Slow subscribers lose data but never impact the session or other
  subscribers — the contract the HTTP surface already exposes.
- The invariant "fan-out is Manager-side, not Session-side" is
  load-bearing. Reversing it means reintroducing per-runtime duplication.
- Ring sizing is a tuning knob, not a correctness knob. Changing it
  does not require a new ADR unless the drop semantics change.
