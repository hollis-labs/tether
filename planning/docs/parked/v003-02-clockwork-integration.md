# Sprint v003-02 — Clockwork Integration

Epic: [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)

**Epic:** [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)
**Goal:** Let Clockwork dispatch tasks through the mux API and inspect session state. Establish the boundary: Clockwork owns planning/scheduling/queue policies; mux owns launch/run/attach/events. Add the minimal surfaces Clockwork needs to replace filesystem-poking with API calls.
**Exit criteria:**
- [ ] Clockwork can call `POST /sessions` with a launch spec and receive a session ID.
- [ ] Clockwork can call `GET /sessions/{id}` and `GET /sessions/{id}/events` for status.
- [ ] Clockwork can stop a session it dispatched via `POST /sessions/{id}/stop`.
- [ ] A reference "dispatch an agent" workflow works end-to-end from Clockwork's perspective.
- [ ] Boundary is respected: mux gains no scheduling / queue / escalation logic in this sprint.

## Context

Context-pack §02 splits responsibilities: "Clockwork decides / Agent Mux runs." Context-pack §03 v0.0.3 bullets include "Clockwork can launch and inspect sessions through Agent Mux." Context-pack §04 use case 5 (Clockwork project manager / planner) is the target scenario; v0.0.3 delivers the primitives it needs, not the full agent.

Current state:
- Mux API (Sprint v002-05) exposes sessions + events endpoints Clockwork can use.
- Clockwork is a separate repo not inspected for this plan. Like v003-01, this sprint is thin at capture time and expects readiness refinement.

## Tasks

### T-v003-s02-01: Clockwork-side mux client

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [integration, clockwork, client]

#### Problem

Clockwork has no mux client. Today Clockwork dispatches by writing files or invoking shell commands — this sprint replaces those paths with API calls for mux-backed dispatch.

#### Fix direction

- Inside Clockwork, create a `muxclient` module that wraps the discovery (shared with Nanite, from T-v003-s01-01) and the API calls it needs: `Launch(spec) → sessionID`, `GetSession(id)`, `StopSession(id)`, `SubscribeEvents(filter)`.
- Leave the legacy filesystem dispatch path in place (flag-gated) until the mux-backed path is proven.

#### Files

- Clockwork repo — path TBD.
- Possibly `pkg/client/` in mux if extraction is warranted (shared with Nanite).

#### Acceptance criteria

- [ ] Clockwork can import the client and call each listed method.
- [ ] Integration test: Clockwork dispatches a launch, polls state, sees completion.

#### Test plan

- Unit in Clockwork: mocked mux server.
- Integration: both daemons running, end-to-end dispatch.

#### Scope fences

- Do not implement retry/backoff logic here — base client is a thin wrapper. Retries live in the dispatch policy layer in Clockwork.
- Do not absorb mux runtime logic into Clockwork.

#### Relationship

Depends on: T-v002-s05-01 (mux API). Possibly shares work with T-v003-s01-01.
Blocks: T-v003-s02-02.

#### Origin

Context-pack [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md), [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) Clockwork responsibilities.

---

### T-v003-s02-02: Clockwork dispatch path uses mux

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [integration, clockwork, dispatch]

#### Problem

Clockwork's existing task dispatch (assigning a task to a worker agent) doesn't go through mux. Until it does, Clockwork can't use mux-owned detached sessions, events, or broker envelopes.

#### Fix direction

- Add a dispatch mode: "mux-backed" vs "legacy." Task definitions indicate which mode to use.
- For mux-backed tasks: Clockwork builds a launch spec, POSTs to mux, captures session ID, tracks via events.
- When task completes (or fails), Clockwork updates its own task state from mux events, not from filesystem polling.

#### Files

- Clockwork repo — dispatch engine path TBD.

#### Acceptance criteria

- [ ] At least one task type (e.g., a worker-agent task) runs in mux-backed mode end-to-end.
- [ ] Completion/failure state propagates from mux events to Clockwork's task state.
- [ ] Legacy mode still works for tasks that haven't migrated.

#### Test plan

- Manual end-to-end: create a task, dispatch it, watch it run in mux, see Clockwork reflect completion.

#### Scope fences

- Do not migrate every task type in this sprint. One reference task type is enough to prove the path.
- Do not build escalation/retry logic here — v0.1.
- Do not touch operational-agent support (planner/operator) — v0.1.

#### Relationship

Depends on: T-v003-s02-01.

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) Clockwork responsibilities, [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 5.

---

### T-v003-s02-03: Inspection surface: Clockwork views mux session detail

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [integration, clockwork, observability]

#### Problem

When a dispatched session is running or stuck, a human using Clockwork's UI/CLI should see enough to decide what to do (attach, stop, re-queue).

#### Fix direction

- Add a "mux session" panel/command to Clockwork: shows state, logical agent, recent events, attach link.
- For live attach, redirect to `mux sessions attach <id>` (CLI) or to the Nanite attach pane (UI) rather than building a third attach client in Clockwork.

#### Files

- Clockwork repo — inspection UI path TBD.

#### Acceptance criteria

- [ ] From a Clockwork task view, the user can see the backing mux session ID, state, and recent events.
- [ ] The user can trigger stop from Clockwork.
- [ ] The user is pointed at Nanite / CLI for attach rather than a third attach implementation.

#### Test plan

- Manual: dispatch, inspect from Clockwork, stop.

#### Scope fences

- Do not build a Clockwork-native attach pane. Use Nanite or CLI for that.
- Do not duplicate `GET /events/stream` subscriber logic — share the client from T-v003-s02-01.

#### Relationship

Depends on: T-v003-s02-02.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 2, use case 5.

## Review / readiness notes

- **Clockwork repo inspection is prerequisite.** Before this sprint's tasks get concrete File pointers, inspect the Clockwork codebase. Readiness gap.
- **Shared client package:** if both Nanite and Clockwork want a Go client, extract to `pkg/client/` in the mux repo. If Clockwork is in a different language (TypeScript, Python), clients stay in their respective repos.
- **Boundary discipline:** this sprint's biggest risk is scope creep — adding "just a little" scheduler logic into mux to make the integration smoother. Resist. Every feature request goes through: "does this belong in Clockwork?"
