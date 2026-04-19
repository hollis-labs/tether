# internal/runtime

Session registry, attach broker, and lifecycle manager. The long-lived
counterpart to the thin `internal/app` composition root — if you feel
the urge to put lifecycle logic in `app`, you're in the wrong package.

**Purpose:** owns running session handles across their full lifetime.
One `Manager` per daemon process; all registry access is mutex-guarded.
Multiplexes PTY output to any number of attach subscribers via a
ring-buffered broker.

**Entry points:**

- `Manager` (`manager.go`) — `NewManager(sink).WithAttachmentSink(...)
  .WithEventPublisher(...)`. Methods: `Start`, `Stop`, `Get`, `List`,
  `WaitSession`, `AttachWith`, `SendInput`, `Shutdown`.
- `attachBroker` (`attach.go`) — per-session ring buffer +
  drop-oldest fan-out. Exposed indirectly through Manager.Attach;
  callers never hold one directly.
- `computeReplay` (`attach.go`) — pure function used to derive the
  `since_seq` catch-up window.
- `StartRequest`, `SessionInfo`, sentinel errors (`ErrManagerStopped`,
  `ErrSessionNotRunning`).

**Neighbors:**

- [`internal/app`](../app) constructs the Manager and passes in
  `store.Store` as the `StateSink` + `AttachmentSink` + event
  `Persister`. See [ADR 0002](../../docs/adr/0002-daemon-transport.md)
  for the daemon design decision that makes this package necessary.
- [`internal/provider`](../provider) provides the `Runtime` +
  `Session` interfaces Manager operates over.
- [`internal/api`](../api) calls Manager through the `LaunchService`
  interface; attach endpoints use `AttachWith(ctx, id, w, opts)`.
- [`internal/events`](../events) receives `session.state_changed`
  events at every transition.

**Gotchas:**

- **Attach fan-out is Manager-side, not Session-side.** CLI and API
  sessions both write to `StartOptions.Fanout`; the broker multiplexes.
  Do not add `Attach(ctx, w)` to `provider.Session` — it duplicates the
  broker and breaks symmetry.
- The watch goroutine is session-scoped, not request-scoped. Do not
  tie it to the `Start` ctx — it needs to survive.
- `SendInput` is serialized per-session via a per-entry `inputMu`.
  Multiple concurrent writers never interleave.
- Slow attach subscribers drop bytes; they never block the PTY reader.
- Terminal state (completed / failed / killed) closes the broker so
  subscribers drain cleanly.
