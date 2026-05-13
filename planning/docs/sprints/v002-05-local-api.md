# Sprint v002-05 — Local API

Epic: [v0.0.2](../epics/v0.0.2-runtime-foundation.md)

**Epic:** [v0.0.2](../epics/v0.0.2-runtime-foundation.md)
**Goal:** Expose a stable local API (HTTP on loopback, optionally Unix socket) that implements the endpoints sketched in context-pack §08. Covers session lifecycle, attach SSE/WebSocket, send-input, checkpoint create/list, broker envelope post/list. Endpoints return well-formed payloads even where downstream semantics are skeletal (resume flow in v0.0.3, request/reply semantics in v0.0.3).
**Exit criteria:**
- [x] HTTP server runs inside the daemon (from Sprint v002-01) on a configurable loopback port (default `127.0.0.1:7180`).
- [x] Optional Unix socket at `$XDG_RUNTIME_DIR/agent-mux/muxd.sock` (or `~/.agent-mux/run/muxd.sock`). (Actual default is UDS at `~/.agent-mux/run/muxd.sock`; TCP on loopback available via `listen_addr: tcp:127.0.0.1:PORT`.)
- [x] Session endpoints implemented: `POST /sessions`, `POST /sessions/{id}/launch`, `GET /sessions`, `GET /sessions/{id}`, `POST /sessions/{id}/stop`, `POST /sessions/{id}/input`, `GET /sessions/{id}/attach`. (Attach ships as `application/octet-stream` per D2, not SSE — SSE is reserved for `/events/stream`.)
- [x] Checkpoint endpoints implemented (skeletal): `POST /sessions/{id}/checkpoint`, `GET /logical-agents/{id}/checkpoints`, `POST /logical-agents/{id}/resume` (returns 501 NotImplemented until v0.0.3).
- [x] Broker endpoints implemented (skeletal persistence): `POST /broker/envelopes`, `GET /broker/envelopes`, `GET /broker/envelopes/{id}`, `POST /broker/envelopes/{id}/reply`.
- [x] Event endpoints implemented: `GET /events/stream` (SSE), `GET /sessions/{id}/events`.
- [x] All responses are JSON; events use SSE. Attach uses raw octet-stream per D2.
- [x] OpenAPI / Markdown API reference checked in under `docs/api/` or similar. (Markdown at `agent-mux/docs/api/README.md`; OpenAPI deferred per sprint scope fence.)

## Context

Context-pack §08 is the source of truth for endpoint shapes. Context-pack §05 says "Expose a minimal but stable local API for clients. Possible initial operations: create session, launch session, list sessions, get session, attach stream, send input, stop session, create checkpoint, list checkpoints, send broker message, list broker messages/events." Context-pack §07 targets `internal/api` for handlers/transport.

Current shape to evolve:
- No HTTP surface exists in the repo yet.
- Sprint v002-01 adds a daemon with a `/health` listener — this sprint fills that listener with real endpoints.
- `runtime.Manager` (v002-s01-01) is the backing runtime; `store` (v002-s03) is the backing persistence.

When done, Nanite and Clockwork have a stable surface to build integrations against in v0.0.3.

## Tasks

### T-v002-s05-01: HTTP router + session lifecycle endpoints

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, api, http]

#### Problem

No HTTP surface exists beyond the `/health` stub from v002-01-02. Session lifecycle endpoints are the foundation every other endpoint depends on.

#### Fix direction

- Pick a router. Stdlib `net/http` + `http.ServeMux` is enough for v0.0.2; `go-chi/chi` is a reasonable upgrade if middleware layering becomes painful. Default: chi (clean path params, middleware pipeline). Capture as ADR.
- Create `internal/api/server.go` with `NewServer(mgr *runtime.Manager, store *store.Store) http.Handler`.
- Implement handlers in `internal/api/sessions.go`:
  - `POST /sessions` — accepts `{launch_id}` (or full launch plan for ad-hoc), creates a session record, returns session ID without starting.
  - `POST /sessions/{id}/launch` — transitions from `created` to `running` (calls `runtime.Manager.Start`).
  - Shortcut: `POST /sessions:create-and-launch` for the common case.
  - `GET /sessions` — list with pagination (`?limit=&cursor=`) and filter by state.
  - `GET /sessions/{id}` — single session with plan, attachments count.
  - `POST /sessions/{id}/stop` — graceful stop with optional `{force: bool}`.

Response envelope: `{data: ..., error: {code, message}?}` or the RFC 7807 problem-detail shape — pick one and stick with it.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/server.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/sessions.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/errors.go`
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/daemon.go` — mount the API server on the daemon listener

#### Acceptance criteria

- [x] `curl -X POST http://127.0.0.1:7180/sessions -d '{"launch":"demo-launch"}'` returns 201 with a session ID. (Verified via UDS smoke — path + request key `launch` per implementation; creates in state=created only per D3 split.)
- [x] `curl http://127.0.0.1:7180/sessions` returns a JSON list. (Plus `?limit=`, `?cursor=`, `?state=` pagination/filter landed in T-s05-01c.)
- [x] `curl -X POST http://127.0.0.1:7180/sessions/{id}/stop` terminates the session.
- [x] Error responses use the chosen envelope consistently. (D4: `{error:{code,message}}` typed envelope, applied across every handler.)
- [x] The CLI client from T-v002-s01-03 now issues real requests. (It always did; updated to use split create+launch + new error envelope.)

#### Test plan

- Unit: handler tests using `httptest.NewServer` + stubbed `runtime.Manager`.
- Integration: daemon up, curl through the lifecycle, assert state in SQLite.

#### Scope fences

- Do not add authentication in v0.0.2 — loopback-only is the trust boundary. Auth is v0.3+.
- Do not add rate-limiting or quotas.
- Do not expose `internal/store` directly; always go through `runtime.Manager` for runtime-affecting ops.

#### Relationship

Depends on: T-v002-s01-01, T-v002-s01-02, T-v002-s03-01.
Blocks: T-v002-s05-02, T-v002-s05-03, T-v002-s05-04.

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) session operations, [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) API guidance.

---

### T-v002-s05-02: Attach SSE + send-input endpoints

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, api, attach, input]

#### Problem

Clients (CLI, Nanite, Clockwork) need a wire protocol for live attach and input. SSE is the simplest fit for append-only output streams; WebSocket is heavier but allows bidirectional flow.

#### Fix direction

- `GET /sessions/{id}/attach` — `Content-Type: text/event-stream`. Each SSE event is one chunk of PTY output. Event name `data` for bytes, `event` for lifecycle transitions.
  - Query params: `?since_seq=N` for resume (keyed off the log ring buffer or persistent `events` stream_seq).
  - Connection: the handler subscribes to the attach broker (T-v002-s02-01) and writes as bytes arrive.
- `POST /sessions/{id}/input` — JSON body `{data: "base64…", raw: bool}` or raw `application/octet-stream` body. Both acceptable; decide one as the canonical path. Recommend: JSON envelope with base64 for cross-client safety.
- Detach semantics: closing the SSE connection detaches the client; runtime continues.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/attach.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/input.go`

#### Acceptance criteria

- [x] `curl -N http://127.0.0.1:7180/sessions/{id}/attach` streams PTY output continuously. (Verified in Sprint 2 smoke; still passes.)
- [x] `curl -X POST http://127.0.0.1:7180/sessions/{id}/input -d '...'` delivers input to the session. (Body is raw octet-stream per D2, not JSON-base64 envelope — decision locked pre-sprint.)
- [x] Closing the curl pipe does not kill the session. (Sprint 2 already verified; attach broker's per-subscriber cancel doesn't affect others.)
- [x] Two concurrent attach calls both receive output. (Sprint 2 smoke pinned this.)
- [x] `?since_seq=N` replays recent history before switching to live. (Interpretation: byte-offset since broker init. `computeReplay` pure function covers at-head / past-head / inside-ring / gap cases.)

#### Test plan

- Unit: SSE handler wired to a fake broker; assert event framing.
- Integration: full loop with daemon, launch, attach, input, verify.

#### Scope fences

- Do not add WebSocket alongside SSE in v0.0.2 — one transport is enough. WebSocket can be added later if UX needs it.
- Do not implement binary-safe output framing beyond SSE's base64-or-escape convention.
- Do not add back-pressure control beyond the Go channel default — if a slow client falls behind, drop them.

#### Relationship

Depends on: T-v002-s02-01 (attach broker), T-v002-s02-02 (SendInput), T-v002-s05-01.

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) `GET /sessions/{id}/attach`, `POST /sessions/{id}/input`.

---

### T-v002-s05-03: Checkpoint endpoints (skeletal)

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, api, checkpoint]

#### Problem

Clients need an API shape to start building checkpoint-aware flows in v0.0.3. Without skeletal endpoints, v0.0.3 can't integrate.

#### Fix direction

- `POST /sessions/{id}/checkpoint` — accepts checkpoint payload, inserts a row via `store.checkpoints`, returns the new checkpoint ID. No runtime side-effects in v0.0.2 (no session stop, no snapshot capture).
- `GET /logical-agents/{id}/checkpoints` — list checkpoints for a logical agent.
- `POST /logical-agents/{id}/resume` — accepts `{checkpoint_id, launch_overrides?}`, returns HTTP 501 with a clear body: `{error:{code:"not_implemented",message:"resume lands in v0.0.3"}}`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/checkpoints.go`

#### Acceptance criteria

- [x] Create checkpoint returns 201 with ID; row persisted. (UUIDv7 server-assigned; `logical_agent_id` resolved from the session row; `source_session_id` set to path param.)
- [x] List returns checkpoints in descending `created_at` order. (Store method already ordered this way per Sprint 3.)
- [x] Resume returns 501 with the documented body (`{"error":{"code":"not_implemented","message":"resume lands in v0.0.3"}}`).

#### Test plan

- Unit: handler tests using a stubbed store.

#### Scope fences

- Do not implement resume. This is v0.0.3 (Sprint v003-04).
- Do not snapshot session context on checkpoint — that's also v0.0.3.
- Do not define the full checkpoint payload schema rigidly. Accept a loose JSON object in v0.0.2.

#### Relationship

Depends on: T-v002-s03-03, T-v002-s05-01.

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) Checkpoint operations.

---

### T-v002-s05-04: Broker endpoints (persistence-only)

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, api, broker]

#### Problem

Broker envelope persistence exists (Sprint v002-03) but there's no way to write or read envelopes from outside the daemon. Sprint v003-05 will add request/reply semantics on top; this sprint delivers the I/O primitives.

#### Fix direction

- `POST /broker/envelopes` — accepts envelope body, inserts a row. Emits an event (via Sprint v002-06).
- `GET /broker/envelopes` — list with filters: `?recipient=`, `?workflow_id=`, `?correlation_id=`.
- `GET /broker/envelopes/{id}` — single envelope.
- `POST /broker/envelopes/{id}/reply` — convenience: inserts a new envelope with `correlation_id` copied from the target, `sender`/`recipient` swapped. No semantic enforcement beyond that in v0.0.2.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/broker.go`

#### Acceptance criteria

- [x] POST returns 201 with envelope ID; row persisted. (UUIDv7 server-assigned; write path is `broker.Service` so `broker.envelope_created` fires on the bus for free.)
- [x] List filters work — exactly-one of `?recipient=` or `?workflow_id=` required (both → 400); `?correlation_id=` scopes workflow queries.
- [x] GET by ID returns the envelope.
- [x] Reply persists a second envelope with swapped sender/recipient and propagated `workflow_id` + correlation (copies original.CorrelationID when set, otherwise seeds with original.ID).

#### Test plan

- Unit: handler tests; SQL-backed round-trip.

#### Scope fences

- Do not implement request/reply waiting semantics (block until response). That's Sprint v003-05.
- Do not route envelopes to sessions (no inter-session delivery). Sessions pull via API; push is v0.0.3.
- Do not define envelope schema validation beyond the store CRUD contract.

#### Relationship

Depends on: T-v002-s03-04, T-v002-s05-01.
Pairs with: T-v002-s06-02 (broker events emitted here are consumed by the event bus).

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) broker operations.

---

### T-v002-s05-05: Event stream endpoints

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, api, events, sse]

#### Problem

External clients (Nanite, Clockwork, CLI) need a way to observe runtime events. Without an event stream, they have to poll `GET /sessions` which is wasteful.

#### Fix direction

- `GET /events/stream` — SSE stream of all runtime events. Query params: `?since_seq=N`, `?scope=session|broker|daemon`, `?session_id=`.
- `GET /sessions/{id}/events` — non-streaming, paginated list of historical events for one session.
- Handler subscribes to `internal/events` bus (Sprint v002-06) and also falls back to `store.events` for historical replay when `since_seq` is behind current.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/events.go`

#### Acceptance criteria

- [x] `curl -N http://127.0.0.1:7180/events/stream` receives session/daemon events live. (Smoke confirmed replay of 3 historical + 1 live broker event during a 2s window.)
- [x] `?session_id=X` filter works. (Plus `?since_seq=`, `?scope=` repeatable.)
- [x] `GET /sessions/{id}/events` returns historical rows. (Newest-first with `?limit=` + `?cursor=` pagination; new `store.ListEventsBySession` method.)
- [x] SSE `id:` field is the `stream_seq` so clients can resume. (Frame: `id:`, `event:`, `data:` with scope + session_id + payload_json envelope; 15s `: ping` keep-alive.)

#### Test plan

- Unit: SSE handler wired to a stubbed bus.
- Integration: daemon up, connect, trigger events, observe stream.

#### Scope fences

- Do not mix event stream with attach stream — they are separate endpoints for separate purposes.
- Do not add server-side event filtering beyond the listed params.
- Do not durably store client subscription offsets — clients track `since_seq` themselves.

#### Relationship

Depends on: T-v002-s06-01 (bus).

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) event operations.

---

### T-v002-s05-06: API reference documentation

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [docs, api]

#### Problem

Nanite and Clockwork adopters (v0.0.3) need a readable reference.

#### Fix direction

- Write `docs/api/README.md` listing every endpoint with method, path, params, example request/response.
- Optional: OpenAPI 3.1 spec in `docs/api/openapi.yaml` if it doesn't feel premature.
- Link from the top-level `roadmap.md` and from the v0.0.3 epic.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux-v0-pack/docs/api/README.md`
- `/Users/chrispian/Projects-apps/agent-mux/docs/api/README.md` (mirror inside the code repo, or pick one canonical location)

#### Acceptance criteria

- [x] Every endpoint added in this sprint is documented with method, path, request shape, response shape. (See `agent-mux/docs/api/README.md`.)
- [x] Cross-references are correct.
- [x] A new reader can `curl` the API using only this doc.

#### Test plan

- Human review: follow the doc against a running daemon.

#### Scope fences

- Do not auto-generate from code in v0.0.2 — manual is fine at this scale.
- Do not write Nanite/Clockwork integration guides here — those belong in their respective repos.

#### Relationship

Depends on: all other tasks in this sprint.

#### Origin

Context-pack [11-task-list-v0-0-2.md](../agent-mux-vfuture-context-pack/11-task-list-v0-0-2.md) milestone 7 (documentation).

## Review / readiness notes

- **Transport decision (HTTP + UDS, HTTP-only, UDS-only)** — context pack hedges. Recommend HTTP-only on loopback for simplicity in v0.0.2; UDS is a trivial addition later. Confirm with readiness review before T-v002-s05-01 starts.
- **Auth posture:** loopback + filesystem permissions on UDS are the trust boundary. No bearer tokens, no mTLS in v0.0.2. Flag for v0.3+ if there's ever a remote-client use case.
- **Input encoding (raw vs JSON base64):** choose one and document. Raw body is simpler for non-JSON clients (netcat); JSON envelope is friendlier for API libraries. Lean JSON envelope.
- **SSE keep-alive:** servers should periodically emit a keep-alive comment (`: ping`) to keep intermediate proxies from closing idle connections. Include in T-v002-s05-02 and T-v002-s05-05.
- **OpenAPI spec** is optional for v0.0.2 — if it slows the sprint, defer to v0.0.3 when Nanite/Clockwork actually adopt.
