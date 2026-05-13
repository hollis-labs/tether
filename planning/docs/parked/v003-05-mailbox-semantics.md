# Sprint v003-05 — Mailbox Semantics

Epic: [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)

**Epic:** [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)
**Goal:** Turn the broker-envelope persistence from v0.0.2 into a working request/reply primitive. Add correlation-ID semantics, workflow-scoped session groups, and the structured envelope types from context-pack §08 (request / response / notice / escalation / handoff / status update). Enable the multiplexor pattern end-to-end.
**Exit criteria:**
- [x] Broker envelopes have a typed message_type enum enforced server-side. *(577a3d7 — 6-type enum, validation in Service + API handler)*
- [x] Request/reply semantics work: a client can post a `request` envelope and wait (with timeout) for a correlated `response`. *(97f5671 — POST /broker/requests with ?wait=true/false, Dispatcher, 504 on timeout)*
- [x] Workflow-scoped session groups exist as a first-class concept in storage and API. *(9b32dff — migration 0009, SessionGroupStore, /session-groups CRUD)*
- [x] The multiplexor use case (one primary session + N siblings) is demonstrable end-to-end via the broker API. *(9982fda — examples/demos/multiplexor/run.sh + README)*
- [x] Correlation-ID scheme is pinned in an ADR. *(577a3d7 — ADR 0018)*

## Context

Context-pack §04 use case 3 (Agent Multiplexor Pattern) is the canonical test: "A primary session delegates to two to four sibling sessions. Communication is brokered through Agent Mux, not via ad hoc files or PTY chatter. Replies are correlated and auditable." Context-pack §08 "Broker semantics" enumerates envelope types and warns: "Do not implement direct PTY-to-PTY free-form chatter. Use structured envelopes."

Current state:
- `broker_envelopes` table (v002-s03-04) has `message_type`, `correlation_id`, `workflow_id` columns but no semantic enforcement.
- Broker API (v002-s05-04) persists envelopes but doesn't wait / correlate.
- Broker events (v002-s06-03) notify subscribers on envelope creation.

## Tasks

### T-v003-s05-01: Enforce envelope types + correlation scheme

**kind:** decision + agent
**priority:** 1
**manual:** true
**tags:** [feature, broker, schema]

#### Problem

Envelope persistence accepts any `message_type`. Correlation IDs are free-form. Without enforcement, clients drift and the request/reply contract breaks.

#### Fix direction

- Define enum in `internal/broker/model.go`: `request | response | notice | escalation | handoff | status_update`. Reject others at the API layer.
- Correlation-ID scheme: recommend UUIDv7 (time-sortable) generated server-side on `request`; `response` must carry the matching `correlation_id`. Capture as ADR.
- Validate on write: `response` requires a `correlation_id` and a referenced `request` envelope that is still open (not already consumed).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/broker/types.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/broker.go`
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0007-broker-correlation.md`

#### Acceptance criteria

- [ ] POSTing an unknown `message_type` returns 400.
- [ ] POSTing a `response` without a valid `correlation_id` returns 400.
- [ ] Correlation ID is UUIDv7 and server-generated for requests.
- [ ] ADR committed.

#### Test plan

- Unit: each validation rule.
- Integration: request/response round-trip lookup.

#### Scope fences

- Do not extend the enum for custom message types. Use `notice` or `status_update` for user-defined messages.
- Do not implement response routing logic here — that's T-v003-s05-02.

#### Relationship

Depends on: T-v002-s03-04, T-v002-s05-04.
Blocks: T-v003-s05-02, T-v003-s05-03.

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) broker semantics, [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 3.

---

### T-v003-s05-02: Request/reply with wait + timeout

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, broker, request-reply]

#### Problem

Clients currently have to poll for responses. Full request/reply means the requester can block (with a timeout) until the matching response arrives — or get a clear timeout.

#### Fix direction

- Add `POST /broker/requests` that wraps `POST /broker/envelopes` with `message_type=request` and blocks until:
  - a matching `response` envelope is posted, or
  - the request-level timeout expires, or
  - the request is explicitly cancelled.
- Implementation: internal in-memory correlation map + event subscription. When a `response` envelope is inserted with matching correlation, the waiting handler unblocks.
- Support async mode: `?wait=false` returns immediately with the request ID, client polls or subscribes.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/broker/dispatcher.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/broker.go`

#### Acceptance criteria

- [ ] Synchronous request blocks until response or timeout.
- [ ] Timeout returns 504 with clear error.
- [ ] Async mode returns 202 with request ID.
- [ ] Multiple concurrent requests don't interfere.

#### Test plan

- Unit: in-memory dispatcher; assert wait/unblock and timeout behavior.
- Integration: two clients, one posts request, other posts response, first unblocks.
- Race: many concurrent requests + responses.

#### Scope fences

- Do not implement persistent request state (survive daemon restart). In-memory is acceptable for v0.0.3; if daemon restarts mid-wait, clients get an error and retry.
- Do not build a queue/retry mechanism. Clients handle retry.

#### Relationship

Depends on: T-v003-s05-01.
Blocks: T-v003-s05-03.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 3, [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md).

---

### T-v003-s05-03: Workflow-scoped session groups

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, broker, workflow]

#### Problem

The multiplexor pattern requires grouping a primary session with its siblings under a shared `workflow_id`. Today `workflow_id` is a free-form column; nothing constrains membership or makes it queryable.

#### Fix direction

- Add `session_groups` table: `id TEXT PRIMARY KEY, name TEXT, created_at TEXT`.
- Add `sessions.session_group_id TEXT REFERENCES session_groups(id)` (nullable).
- Add API: `POST /session-groups`, `GET /session-groups/{id}`, `GET /session-groups/{id}/sessions`, `POST /session-groups/{id}/members` (add session to group).
- On launch, a launch spec can optionally declare a `session_group_id`.
- Filter broker envelopes by group: `GET /broker/envelopes?session_group_id=X`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/0007_session_groups.sql`
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/session_groups.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/session_groups.go`

#### Acceptance criteria

- [ ] Group CRUD works.
- [ ] A session can be added to a group.
- [ ] Broker envelopes scoped to a group are queryable.
- [ ] Multiplexor demo: one primary + two siblings in a group, primary requests work from siblings, responses flow back.

#### Test plan

- Unit: group CRUD, membership.
- Integration: multiplexor demo end-to-end.

#### Scope fences

- Do not implement group-level policies (quotas, shared memory scopes). That's v0.1+.
- Do not support session membership in multiple groups in this sprint. One group per session.

#### Relationship

Depends on: T-v003-s05-01.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 3.

---

### T-v003-s05-04: Multiplexor demo + docs

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [demo, docs, broker]

#### Problem

The multiplexor pattern is the headline use case for v0.0.3. Without a working demo, adopters won't know how to assemble it.

#### Fix direction

- Add `examples/demos/multiplexor/` with:
  - Shell script that sets up a group of 3 sessions (1 primary, 2 siblings).
  - A request/reply exchange script demonstrating correlation.
  - A README walking through the flow.
- Link from the v0.0.3 epic and from the API reference (v002-s05-06).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/examples/demos/multiplexor/README.md`
- `/Users/chrispian/Projects-apps/agent-mux/examples/demos/multiplexor/run.sh`

#### Acceptance criteria

- [ ] `bash examples/demos/multiplexor/run.sh` succeeds end-to-end against a running daemon.
- [ ] README shows expected output.

#### Test plan

- Run the demo.

#### Scope fences

- Do not build an orchestration framework around the demo — a shell script with `curl`s is enough.
- Do not include this in CI (it's a user-facing demo, not a regression test).

#### Relationship

Depends on: T-v003-s05-01, T-v003-s05-02, T-v003-s05-03.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 3.

## Review / readiness notes

- **Request wait vs subscribe:** two styles compete. Blocking `POST /broker/requests` is easier for scripts; event-driven subscription is better for long-lived agents. v0.0.3 offers both with `?wait=` param. If one becomes the clear winner, deprecate the other in v0.1.
- **Handoff envelope type** is listed in context-pack §08 but its semantics overlap with Sprint v003-04's `session.handoff` event. Decide: does `handoff` the broker message do anything different? Probably yes: handoff between *logical agents* (cross-agent) vs the same-agent handoff in v003-04. Flag as a readiness gap.
- **Escalation and notice** envelope types are persistence-only in v0.0.3. Routing them to a human queue / Clockwork escalation path is v0.1 work.
- **Persistence of in-flight requests:** if the daemon restarts mid-wait, clients lose their wait. Document and defer durable dispatcher to v0.1.
