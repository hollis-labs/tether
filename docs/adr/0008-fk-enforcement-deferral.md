# ADR 0008: SQLite Foreign-Key Enforcement — Deferred Until Post-Launch

**Status:** Deferred — 2026-04-19
**Reopen trigger:** After v0.0.2 schema stabilizes post-launch, or when
the first cross-table invariant violation hits production data.
**Context:** Cross-sprint (v002-03 storage evolution + v002-05 local API)
**Deciders:** agent-mux v0.0.2 execution session

## Context

`modernc.org/sqlite` enforces `PRAGMA foreign_keys` only when it is
explicitly enabled per connection. v0.0.1 did not set it; v0.0.2 left it
off. Column-level `REFERENCES` declarations in the migration SQL exist
today (e.g., `sessions.logical_agent_id REFERENCES logical_agents(id)`,
`checkpoints.logical_agent_id`, `checkpoints.source_session_id`,
`client_attachments.session_id`) but they are documentation only — the
engine does not block orphan inserts or cascade deletes.

Three approaches were considered:

1. **Enable enforcement now** with an orphan-delete migration. Pros:
   strongest durability guarantee. Cons: v0.0.2's schema is still
   churning (`logical_agents` rename, `events` evolution). Every
   breaking change that wants to drop a row needs to reason about FK
   cascade semantics during a migration, which slows iteration.
2. **Keep enforcement off and drop the `REFERENCES` declarations.**
   Pros: removes a correctness-inconsistent signal. Cons: loses the
   in-schema documentation of the real invariants; future
   contributors reverse-engineer the shape from Go code instead of
   SQL.
3. **Keep enforcement off, keep `REFERENCES` declarations, document the
   deferral.** Pros: the schema tells the truth about the eventual
   invariants; the deferral is visible. Cons: invariants only enforced
   by Go-side validation + discipline; silent orphan rows possible
   during development.

## Decision

**Option 3 — Deferred.** FK enforcement stays off for v0.0.2. Column-level
`REFERENCES` declarations remain as documentation of the target
invariants. Go-side validation at the store layer rejects obvious
violations (e.g., `checkpoints.Create` requires non-empty id +
logical_agent_id + created_at).

The deferral will be reopened when either of these triggers fires:

- **Post-launch schema stability.** Once v0.0.2 ships and the schema is
  no longer churning, enforce the invariants. At that point the
  orphan-delete migration is small (the user's own data, not a fleet).
- **First cross-table invariant violation in real data.** If a
  production user hits an orphan that breaks a read path, enforcement
  leapfrogs the ergonomics calculus.

## Alternatives Considered

- Enabling now: rejected for the reasons above (schema is still
  churning; pivots are cheaper without FK complexity).
- Dropping `REFERENCES` declarations: rejected (loses documentation
  value).

## Consequences

- Writers must discipline themselves: the Go API layer validates
  required fields; the store rejects obvious invalid inserts.
- Readers assume the invariants hold but must not panic if an orphan
  row appears — graceful degradation (e.g., a checkpoint row whose
  `source_session_id` no longer exists in `sessions` should still list
  but not link).
- When the reopen ADR lands, it will include:
  - An `orphan-delete` migration that removes currently-broken rows.
  - `PRAGMA foreign_keys = ON` flipped at `store.Open`.
  - A test pass that flips enforcement in a temporary copy of a real
    user DB to catch any missed orphans before release.
- Until then, this ADR is the paper trail. Do not silently flip
  `PRAGMA foreign_keys` on as a side-quest.
