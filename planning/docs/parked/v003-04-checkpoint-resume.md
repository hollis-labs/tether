# Sprint v003-04 — Checkpoint Resume

Epic: [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)

**Epic:** [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)
**Goal:** Close the loop on the checkpoint primitives introduced in v0.0.2. Make `POST /sessions/{id}/checkpoint` actually capture meaningful continuity state, and make `POST /logical-agents/{id}/resume` actually start a new session from it. Wire into session stop/handoff flows so handoffs between runtime sessions are observable and auditable.
**Exit criteria:**
- [ ] `POST /sessions/{id}/checkpoint` captures a meaningful snapshot (plan, recent events, optional provider hints) and persists it.
- [ ] `POST /logical-agents/{id}/resume` launches a new session with boot context derived from the checkpoint.
- [ ] A running session can be explicitly checkpointed, stopped, and resumed — output from resume reflects the original context.
- [ ] Handoff flow (stop existing session, create checkpoint, resume as a new session) is testable end-to-end.
- [ ] Checkpoint payload schema is pinned in an ADR.

## Context

Context-pack §02 defines Checkpoint with fields (current status, completed work, pending work, key decisions, referenced artifacts, summarized context, next recommendation). Context-pack §03 v0.0.3 bullets include "initial checkpoint resume path." Context-pack §04 use cases 2 (detached background task) and 4 (warm knowledge keeper) depend on this.

Current state:
- `checkpoints` table exists (v002-s03-03).
- `POST /sessions/{id}/checkpoint` persists payloads (v002-s05-03).
- `POST /logical-agents/{id}/resume` returns 501 NotImplemented.
- Provider `Runtime.Session.CheckpointHints()` returns an opaque blob (v002-s04-02).

## Tasks

### T-v003-s04-01: Pin the checkpoint payload schema

**kind:** decision + agent
**priority:** 1
**manual:** true
**tags:** [decision, checkpoint, schema]

#### Problem

v0.0.2 left the checkpoint payload deliberately loose (TEXT columns). For resume to actually work, the payload shape must be known to both the writer (session-stop path) and the reader (resume path).

#### Fix direction

- Define a `CheckpointPayload` Go struct with the fields from context-pack §02:
  - `status` (string enum: active / paused / completed / escalated)
  - `completed_work` (free text or structured list)
  - `pending_work` (structured list)
  - `key_decisions` (structured list with timestamp + note)
  - `referenced_artifacts` (list of paths or object refs)
  - `summary` (free text, model-generated OK)
  - `next_recommendation` (free text)
  - `provider_hints` (opaque `json.RawMessage` for runtime-specific state)
- Persist as JSON in the existing `checkpoints.*` text columns or add a new `payload_json` column.
- Write an ADR.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/checkpoint/payload.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/0006_checkpoint_payload.sql` (if new column)
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0006-checkpoint-payload-schema.md`

#### Acceptance criteria

- [ ] Struct defined with field-level godoc.
- [ ] JSON round-trips cleanly.
- [ ] ADR committed.
- [ ] v0.0.2-persisted rows still load (back-compat).

#### Test plan

- Unit: marshal/unmarshal round-trip.
- Migration test: v0.0.2 checkpoint → v0.0.3 reader.

#### Scope fences

- Do not require every field to be populated — most are optional.
- Do not over-engineer the schema to support every future use case. This is the baseline.

#### Relationship

Blocks: T-v003-s04-02, T-v003-s04-03.

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) Checkpoint entity.

---

### T-v003-s04-02: Implement checkpoint create from running session

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, checkpoint, runtime]

#### Problem

v0.0.2 lets a client POST a payload. v0.0.3 should let the runtime generate the payload automatically from the running session's state.

#### Fix direction

- Add `runtime.Manager.CreateCheckpoint(ctx, sessionID, userPayload) (*Checkpoint, error)`:
  - Merge `userPayload` with runtime-derived fields (session plan, recent events summary, provider hints from `Session.CheckpointHints()`).
  - Persist via `store.checkpoints`.
  - Emit `checkpoint.created` event.
- Extend `POST /sessions/{id}/checkpoint` to call this, so client-supplied payload gets enriched automatically.
- Optionally, allow `POST /sessions/{id}/checkpoint?stop=true` to atomically checkpoint and stop.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/checkpoint.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/checkpoints.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/kinds.go` — add `checkpoint.created`

#### Acceptance criteria

- [ ] Checkpointing a running session captures plan + recent events + provider hints.
- [ ] With `?stop=true`, session ends in terminal state and row references the checkpoint.
- [ ] Event emitted.

#### Test plan

- Unit: stubbed runtime, checkpoint a session, inspect payload.
- Integration: full daemon + API flow.

#### Scope fences

- Do not capture full PTY output into the checkpoint — it's stored in the log file; reference it, don't duplicate.
- Do not invent an LLM-summarization step here. `summary` is user-provided for v0.0.3; model-generated summaries are v0.1.

#### Relationship

Depends on: T-v003-s04-01.
Blocks: T-v003-s04-03.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 2, [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md).

---

### T-v003-s04-03: Implement resume from checkpoint

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, checkpoint, resume]

#### Problem

`POST /logical-agents/{id}/resume` returns 501. The primitives now exist to implement it.

#### Fix direction

- `POST /logical-agents/{id}/resume` body: `{checkpoint_id, launch_overrides?}`.
- Look up the checkpoint; derive a launch spec:
  - Same logical agent.
  - Provider: inherit from the source session's plan unless overridden.
  - Boot prompt: prepend the checkpoint summary + pending work + key decisions to the usual boot prompt (configurable via launch spec).
  - `provider_hints`: passed to the runtime on `Start` for any runtime-specific resume logic.
- Return the new session ID.
- Emit `checkpoint.resumed` event referencing both the checkpoint and the new session.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/checkpoints.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/checkpoint.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/launch/resume.go` (new; builds a resume-aware plan)

#### Acceptance criteria

- [ ] Resume call returns a new session ID.
- [ ] New session starts with the checkpoint context visible in its boot prompt.
- [ ] `events` link: source session → checkpoint → resumed session.
- [ ] Idempotency: resuming the same checkpoint twice creates two distinct sessions (explicit behavior; document).

#### Test plan

- Unit: resume builds the expected plan.
- Integration: launch → checkpoint-and-stop → resume → verify context.

#### Scope fences

- Do not implement automatic resume-on-crash. Resume is explicit via API.
- Do not implement cross-logical-agent resume in this sprint (checkpoint A → resume as agent B). Same agent only.
- Do not deep-copy session workspace on resume — the new session gets its own workspace per usual. Artifacts referenced in the checkpoint remain at their original paths.

#### Relationship

Depends on: T-v003-s04-02.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use cases 2 & 4, [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md) v0.0.3 bullets.

---

### T-v003-s04-04: Handoff flow CLI + events

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, cli, handoff]

#### Problem

The common "wrap up session A, start session B from its context" flow should be a single CLI command, not three API calls by hand.

#### Fix direction

- Add `mux sessions handoff <source-id> [--summary "…"]`:
  1. Checkpoint source session.
  2. Stop source session.
  3. Resume for the same logical agent.
  4. Print the new session ID.
- Emit a `session.handoff` event linking source, checkpoint, and target.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/sessions.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/kinds.go`

#### Acceptance criteria

- [ ] `mux sessions handoff <id>` produces a new session ID.
- [ ] Event audit shows the full chain.
- [ ] Error handling: if any step fails, partial state is clearly reported (session stopped but not resumed, etc.).

#### Test plan

- Manual end-to-end.
- Error injection test.

#### Scope fences

- Do not auto-attach the caller to the new session. That's a UX concern; keep the command scriptable.
- Do not implement handoff *between* logical agents. Same agent only (see T-v003-s04-03 scope).

#### Relationship

Depends on: T-v003-s04-02, T-v003-s04-03.

#### Origin

Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) (handoff envelope type).

## Review / readiness notes

- **Boot-prompt injection format:** how the checkpoint summary gets inlined in the resume boot prompt is a UX call. A simple "## Resumed from checkpoint …\n\n## Summary\n\n…" section is a safe default; adapters may customize.
- **Provider-specific resume semantics** vary. For CLI providers (claude-code), "resume" is really "start fresh with more context." For API providers (Anthropic), true conversation resume could pass the full message history — but v0.0.3 keeps it simple with context-in-boot-prompt. Pin in the ADR.
- **Idempotency key:** resuming twice creates two sessions by design, but readiness review might want a client-provided idempotency key. Evaluate if it becomes painful.
- **Cross-logical-agent handoff** (use case 3 multiplexor pattern hinting at this) is explicitly out of scope here; it belongs in Sprint v003-05's broker handoff semantics.
