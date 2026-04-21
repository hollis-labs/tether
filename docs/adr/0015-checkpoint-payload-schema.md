# ADR 0015 — Checkpoint Payload Schema

**Status:** accepted
**Date:** 2026-04-21
**Supersedes:** —
**Superseded by:** —

## Context

v0.0.2 shipped the `checkpoints` table with TEXT columns for each payload field, deliberately leaving the schema free-form until the resume path clarified requirements. The columns are: `status`, `completed_work`, `pending_work`, `key_decisions`, `referenced_artifacts`, `summary`, `next_recommendation`, `source_session_id`.

Sprint v0.0.4-S04 implements `POST /logical-agents/{id}/resume`, which reads the most recent checkpoint and injects its content into the new session's boot prompt. For this to work reliably, the payload shape must be pinned — both the writer (`POST /sessions/{id}/checkpoint`) and the reader (`resume`) must agree on what the fields mean.

Additionally, a `provider_hints` field is needed to round-trip opaque runtime-specific continuity state (e.g., a claude CLI `session_id` snapshot, a filesystem diff digest).

## Decision

### Checkpoint payload fields

| Field | Type | Meaning |
|-------|------|---------|
| `status` | string enum | Agent's self-reported state: `active`, `paused`, `completed`, `escalated` |
| `completed_work` | free text | What the agent finished since the last checkpoint |
| `pending_work` | free text | Work started but not finished |
| `key_decisions` | free text | Design calls made during the session |
| `referenced_artifacts` | free text | Paths / object refs produced |
| `summary` | free text | High-level narrative; the primary content injected on resume |
| `next_recommendation` | free text | What the agent recommends the next session do first |
| `provider_hints` | JSON text | Opaque runtime-specific blob. Round-tripped from `Session.CheckpointHints()` back into `StartOptions` on resume. Shape is provider-defined; this ADR does not interpret it. |
| `source_session_id` | string | The session that produced the checkpoint |

All fields except `id`, `logical_agent_id`, and `created_at` are optional (SQL NULL → empty string in Go).

### Schema change

A new `provider_hints TEXT` column is added to `checkpoints` via migration 0008. Existing rows get SQL NULL (treated as empty), preserving full back-compat.

### Resume boot-context injection

The `resume` endpoint prepends a rendered context block to the normal boot prompt:

```markdown
## Resumed from checkpoint <id>

**Status:** <status>

**Summary:**
<summary>

**Pending work:**
<pending_work>

**Key decisions:**
<key_decisions>

---

```

Fields that are empty are omitted from the block. The separator `---` keeps the checkpoint context visually distinct from the agent's normal boot prompt.

### Logical-agent launch_id

The resume endpoint must know which launch profile to use for the new session. A `launch_id TEXT` column is added to `logical_agents` via migration 0008 and populated the first time `LaunchSession` runs for that agent. Resume always uses this stored `launch_id`; no launch-override capability in v0.0.4.

## Rationale

**Why not add a `payload_json` column?** The existing columns already map to the context-pack §02 fields and the Go struct reflects them 1:1. Adding a redundant JSON column would require keeping two representations in sync. Keeping individual columns makes SQL queries readable (e.g., list by status) and avoids over-engineering for v0.0.4.

**Why `provider_hints` as TEXT/JSON?** The `provider.Session.CheckpointHints()` contract deliberately returns an opaque blob. Storing it as TEXT JSON preserves the contract without leaking provider internals into the SQL schema.

**Why always resume from the most recent checkpoint?** v0.0.4 is the minimal viable slice. A checkpoint-picker UI deferred to v0.1.

## Consequences

- `checkpoint.Checkpoint` gains `ProviderHintsJSON string`.
- `store.CreateCheckpoint` and the scan paths include the new column.
- `logical_agents` gains `LaunchID string`, set by `app.Service.LaunchSession`.
- Migration 0008 is additive; no data loss.
- The shipped error message `"resume lands in v0.0.3"` is replaced by a real implementation.

## Follow-ups

- Checkpoint-picker UI (select which checkpoint to resume from) — deferred to v0.1.
- Provider-hints consumption on resume (pass `provider_hints` back into `StartOptions`) — deferred; requires per-provider resume semantics.
- Cross-agent resume (checkpoint A → resume as agent B) — deferred.
