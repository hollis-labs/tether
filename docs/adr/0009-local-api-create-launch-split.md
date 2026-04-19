# ADR 0009: Local API — Split `CreateSession` from `LaunchSession`

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-05 (Local API), task T-v002-s05-01b
**Deciders:** agent-mux v0.0.2 execution session

## Context

v0.0.1's `app.Service.Launch(catalogRoot, launchID)` did four things
atomically: resolve the launch plan, materialize the workspace, persist
the session row, and start the runtime. The HTTP surface inherited the
shape: a single `POST /sessions?launch=<id>` that either succeeded at
all four or failed somewhere in the middle.

Problems surfaced during Sprint v002-05's readiness pass:

1. External clients (Nanite, Clockwork) want to inspect a prepared
   session — its workspace paths, its resolved plan — *before*
   committing to run it. A one-shot Launch denies them that.
2. Workspace materialization is observable side-effect (files on disk,
   rows in the DB) even if the runtime Start fails. The failure mode
   was "half-created" — a session row in state `created` with no live
   runtime and no clear recovery path.
3. The two-endpoint shape matches the internal semantics: `create` is a
   plan+persist; `launch` is a runtime operation.

## Decision

Split into two endpoints and two service methods:

```
POST /sessions                → app.Service.CreateSession(launchID)
  └─ resolves plan, materializes workspace, persists row in state=created
  └─ returns the created Launched{SessionID, Workspace, Plan}

POST /sessions/{id}/launch    → app.Service.LaunchSession(sessionID)
  └─ rehydrates plan via store.GetLaunchPlan
  └─ runtime.Prepare + runtime.Manager.Start
  └─ returns the same Launched{} with a Wait() closure
  └─ ErrSessionNotCreated (→ HTTP 409) if state != "created"
```

- `client.Launch` is retained as a convenience that calls both
  primitives sequentially, preserving the original single-call UX for
  the CLI.
- `workspace.Open(root, sessionID)` is a new non-destructive reconstructor
  that lets `LaunchSession` pick up an already-materialized workspace
  without racing with `workspace.Create`.
- `store.GetLaunchPlan` is a new read method so Launch doesn't need to
  re-resolve from the catalog (which may have changed since Create).

## Alternatives Considered

- **Single endpoint with a `dry_run` flag.** Rejected: flags produce
  multi-state endpoints that are hard to reason about. The HTTP surface
  wants verb-per-operation.
- **Create is idempotent / Launch is retry-safe.** Rejected for v0.0.2:
  the session ID is server-assigned UUIDv7, so "idempotent create" has
  no natural key. We punt on retry semantics until v0.0.3 when clients
  that care about retries (Nanite, Clockwork) land.
- **Keep monolithic Launch, add inspection endpoints on top.**
  Rejected: inspection without an intermediate durable state means the
  plan + workspace exist only as in-memory previews, which is a
  different API.

## Consequences

- A `created` session is first-class. Clients can inspect and abandon
  it; `LaunchSession` is a separate, later operation.
- v0.0.2 does not support relaunch of a session that has transitioned
  past `created`. A client that needs retry creates a new session. This
  keeps the state machine simple: `created → launching → running →
  {completed | failed | killed}`.
- Two round-trips for the common "create + launch immediately" flow —
  `client.Launch` hides this from CLI users; API clients accept the
  cost in exchange for inspection power.
- Related ADRs: [0010](0010-local-api-typed-error-envelope.md),
  [0011](0011-local-api-attach-transport.md).
