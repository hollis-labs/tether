# Sprint v004-04 — Checkpoint Resume (MVP slice)

Epic: [v0.0.4](../epics/v0.0.4-dogfood-ready-infrastructure.md)
Lineage: adapted from parked [[Integration Foundation]] Sprint 4 — see `docs/parked/v003-04-checkpoint-resume.md` for the full version which includes handoff + mailbox integration that v0.0.4 does NOT pull forward.

**Reordered 2026-04-19** — was v004-03; became v004-04 when the claudestream-provider sprint slotted in ahead. Scope unchanged. Note: this sprint ships agent-mux's *generic* checkpoint resume. It is separate from claude's built-in `--resume <session_id>` continuity, which is handled inside Sprint v004-02 T-05.

**Goal:** Close the loop on the checkpoint primitives from v0.0.2. Make `POST /sessions/{id}/checkpoint` capture a useful snapshot, and make `POST /logical-agents/{id}/resume` start a new session with that snapshot as boot context. Users can checkpoint → stop → resume without losing continuity.

**Exit criteria:**
- [x] `POST /sessions/{id}/checkpoint` accepts a typed `CheckpointPayload` and persists it. *(bb727fc — ProviderHintsJSON added; existing endpoint works)*
- [x] `POST /logical-agents/{id}/resume` starts a new session with the latest checkpoint's payload injected into the boot prompt. *(d3e6310)*
- [x] End-to-end: checkpoint a running session → stop it → resume → new session reflects the original context. *(ResumeLogicalAgent builds context-prefixed boot prompt)*
- [x] ADR 0015 pins the CheckpointPayload schema. *(bb727fc)*
- [x] TUI session detail gains a `c` key to trigger a checkpoint. *(0b76f41 — CheckpointModal + c binding + toast)*
- [x] `make check` green. *(403 tests under -race, 0 issues)*

## Out of scope (deferred vs parked sprint)

The parked Integration Foundation Sprint 4 also covered:
- Handoff flow (stop existing → checkpoint → resume as new, as one atomic-ish sequence)
- Auto-checkpoint on stop
- Mailbox envelope correlation with resume events

v0.0.4 intentionally ships only the round-trip so dogfooding can rely on "start a session, checkpoint it, restart, resume." The richer handoff flow belongs to the unparking of Integration Foundation proper.

## Tasks

### T-v004-s03-01: Pin CheckpointPayload schema + ADR 0015

**Priority:** 1. **Tags:** schema, decision, adr.

Copy the task shape from `docs/parked/v003-04-checkpoint-resume.md` T-01, with these adaptations:
- ADR is **0015** (0013 = sandboxing, 0014 = pty-resize, per v0.0.4 ordering).
- Pay attention to `provider_hints` — the v0.0.2 opaque blob stays opaque; this sprint does not interpret it, only round-trips it.

### T-v004-s03-02: Implement `POST /logical-agents/{id}/resume`

**Priority:** 1. **Tags:** daemon, api, resume.

Copy from `docs/parked/v003-04-checkpoint-resume.md` T-02 with these scope narrowings:
- Resume builds a new session from the **most recent** checkpoint of the logical agent (no selection UI yet).
- Resume uses the original launch profile (from `logical_agents.launch_id`) unchanged — no overrides yet.
- The boot-context injection format is a templated prompt prefix: the payload is rendered into a markdown block prepended to the normal boot prompt.

### T-v004-s03-03: Wire checkpoint trigger into session detail

**Priority:** 2. **Tags:** tui, ux.

#### Problem
Even with the checkpoint endpoint live, users need a way to trigger one from the TUI.

#### Fix direction
- Session detail adds a `c checkpoint` keybinding.
- On press, opens a simple form modal (reuse `internal/tui/modal/` pattern) with two fields: `status` (select from active/paused/completed/escalated) and `summary` (multi-line input). Leaves `completed_work`, `pending_work`, `key_decisions`, `referenced_artifacts`, `next_recommendation` empty for v0.0.4 — a free-text summary is the MVP payload.
- Submit → `POST /sessions/{id}/checkpoint` → toast on success/failure.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/session.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/modal/form.go` (new; light form composer — single input + a select)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/client.go` (CreateCheckpoint method)

#### Acceptance criteria
- [ ] `c` on session detail opens the modal.
- [ ] Submitted checkpoint appears in the session's checkpoint list on reload.
- [ ] Toast confirms success or reports error.

#### Scope fences
- Do not build a full form framework — this is a narrow two-field modal. Sprint 3 of the re-slotted work will generalize it.
- Do not auto-suggest content; user fills in manually.

---

### T-v004-s03-04: Resume trigger on logical-agent row

**Priority:** 2. **Tags:** tui, ux.

#### Problem
Resume isn't reachable from the TUI without CLI plumbing.

#### Fix direction
- Main-screen: when a logical-agent row type exists (add to `RowType` enum + catalog loader if not already), Enter on it triggers resume via `POST /logical-agents/{id}/resume`.
- On success, auto-attach into the new session (same pattern as launch).
- Readiness gap: if logical-agents aren't already in the main-screen row mix, adding them is its own small task. Verify during readiness pass; may defer this task if scope grows.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/row.go` (add LogicalAgentRow if needed)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/main_screen.go` (dispatch in handleEnter)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/client.go` (ResumeLogicalAgent)

#### Acceptance criteria
- [ ] Enter on a logical-agent row with a checkpointable history triggers resume.
- [ ] User lands in the new session via auto-attach.

#### Scope fences
- Do not let user pick a specific checkpoint to resume from — always use the most recent.
- Do not generalize to "resume any session" — resume is logical-agent-scoped, not session-scoped.

---

## Review / readiness notes

- **Logical-agent rows on main screen.** Confirm whether v0.0.3 Sprint 5 was going to add them. If it was, pull forward only the minimum (the row type + list) into this sprint. Otherwise T-v004-s03-04 needs a preceding task for that addition; capture as readiness note.
- **Boot-context injection format.** The prompt template might live in `internal/provider/cli/claudecode/adapter.go` bootstrap logic. Verify during T-02 readiness that we can prepend a context block without breaking the claudecode provider's own boot-prompt handling.
- **Back-compat with v0.0.2-persisted checkpoints.** Existing rows were written before the payload schema was pinned. Resume logic should tolerate legacy rows (treat missing fields as empty).
