# internal/api

HTTP handlers that serve the daemon's local API over a Unix domain
socket (or TCP for debugging).

**Purpose:** sessions CRUD (create / launch / list / get / stop),
attach with `since_seq` resume, input injection, checkpoint endpoints,
broker envelope endpoints, and the events stream + history. Typed
error envelope everywhere: `{error:{code,message}}`.

**Entry points:**

- `NewHandler(Deps)` (`server.go`) — assembles the `http.Handler`
  with routes gated by which Deps are non-nil (Bus, EventsStore,
  Checkpoints, Broker).
- `server.go` + `sessions.go` — sessions routes, pagination,
  state filter.
- `attach.go` — live-stream PTY output with `since_seq=<int>` resume.
- `input.go` — raw body POST (1 MiB cap) to `/sessions/{id}/input`.
- `checkpoints.go` — skeletal POST/GET; resume returns 501 until v0.0.3.
- `broker.go` — envelope CRUD; reads require `recipient` xor
  `workflow_id` filter.
- `events.go` — SSE `/events/stream` + historical list per session.
- `errors.go` + `types.go` — typed error codes (`invalid_request`,
  `not_found`, `method_not_allowed`, `payload_too_large`, `conflict`,
  `internal_error`, `not_implemented`) and DTOs.

**Neighbors:**

- Mounted onto the HTTP mux by [`internal/daemon`](../daemon). The
  daemon package owns the listener, PID file, and lifecycle.
- Talks to [`internal/app`](../app) through the `LaunchService`
  interface (create / launch / list / get / stop / attach / input).
- Events handlers consume [`internal/events`](../events).Bus and the
  store-backed `EventsStore`.
- Checkpoint + broker handlers call [`internal/store`](../store) and
  [`internal/broker`](../broker).

**Gotchas:**

- **Attach is `application/octet-stream`, not SSE.** SSE is reserved
  for the events stream. Don't try to unify them.
- `since_seq=0` replays the full ring; `since_seq=` (empty) is
  rejected as a malformed request.
- All writes that fan-in to the broker emit events "for free" via
  `broker.Service`. Don't bypass the service to save a line of code.
- Routes are only registered when their dependency is non-nil — this
  is how `NewHandler(Deps{Service: svc})` yields a 404 for
  `/events/stream` when no Bus is configured.
- Full reference: [`docs/api/README.md`](../../docs/api/README.md).
