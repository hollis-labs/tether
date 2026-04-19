# ADR 0001: Migration Framework

**Status:** Accepted — 2026-04-18
**Context:** Sprint v002-03 (Storage Evolution), task T-v002-s03-01
**Deciders:** agent-mux v0.0.2 execution session

## Context

v0.0.1 applied `schema.sql` as a single `db.Exec` blob on every startup,
relying on `CREATE TABLE IF NOT EXISTS` for idempotency. v0.0.2 needs to
evolve the schema across multiple sprints (`client_attachments` here,
`logical_agents` / `checkpoints` / `broker_envelopes` / `events` evolution
in later Sprint 3 tasks). Idempotent-blob loses signal once migrations add
columns or indexes to existing tables — `ALTER TABLE` is not idempotent in
SQLite without custom guards, and reasoning about "what state is the DB
in?" gets harder with every new statement.

Three candidates were considered:

1. **Option A — Embedded `.sql` files + hand-rolled versioning table**
   Each migration is a numbered file applied once, tracked in a
   `schema_migrations` table. Minimal dependency footprint (just stdlib +
   `modernc.org/sqlite`, already in use). Full control over the adoption
   path for existing v0.0.1 databases.

2. **Option B — `github.com/pressly/goose`**
   Mature, Go-native, supports `modernc.org/sqlite`. Adds a dependency
   and a small CLI we don't need; brings up/down semantics we've
   explicitly scoped out (forward-only for v0.0.2).

3. **Option C — `github.com/golang-migrate/migrate`**
   Broader ecosystem (Postgres, MySQL, etc.), heavier, more abstractions
   per our needs. Overkill at this scale.

Sprint readiness notes (per sprint v002-03 plan) recommend Option A
"unless readiness review wants ecosystem compatibility now." We don't —
v0.0.2 is the first time the schema is churning and we want to keep the
dependency surface tight.

## Decision

**Option A.** Embedded migrations + hand-rolled `schema_migrations` table.

- Migrations live in `internal/store/migrations/NNNN_description.sql`,
  zero-padded to 4 digits. They are embedded via `//go:embed`.
- `schema_migrations` table: `(version INTEGER PRIMARY KEY, name TEXT,
  applied_at TEXT)`.
- On `store.Open`:
  1. Create `schema_migrations` if missing.
  2. Load applied versions.
  3. For each embedded migration in version order: if unapplied, run in
     a transaction and insert the `schema_migrations` row.
- **v0.0.1 adoption path.** If the DB already contains a `sessions`
  table but no `schema_migrations` row for version 1, the migrator
  stamps version 1 without re-running `0001_init.sql`. This is safe
  because `0001_init.sql` is a faithful port of the v0.0.1 schema and
  would be a no-op anyway; stamping makes the detection explicit and
  avoids confusing logs.
- Forward-only. No rollback. If a migration is buggy, it's reverted by
  a new higher-numbered migration.

## Consequences

**Positive**

- Zero new dependencies. `embed` + `database/sql` is enough.
- Reasoning about state is trivial: read `schema_migrations`, compare to
  embedded file list.
- Test surface is small: inject a `fs.FS` into the migrator core and
  unit tests can exercise adoption, ordering, idempotency without
  touching real SQL files.

**Negative**

- No down-migrations. Acceptable for v0.0.2 (pre-launch, small blast
  radius); revisit if production data requires rollback.
- No multi-statement safety net beyond `BEGIN ... COMMIT`. Each migration
  file runs as a single transaction; if a file contains DDL that SQLite
  implicitly commits (rare — mostly `VACUUM` / pragmas), we handle it in
  the migration's own SQL, not the framework.
- If we later outgrow this and need branching / squashing, we'll migrate
  to `goose` or equivalent. The file-naming convention is already
  compatible with both.

## Notes for future ADRs

- Column-add migrations may need `ALTER TABLE ... ADD COLUMN` with a
  backfill step. Do the `ALTER` + `UPDATE` in a single migration file,
  inside the transaction.
- If migrations start needing conditional logic (detect existing state),
  prefer writing Go-side pre-check in the migrator and splitting into
  two SQL files (one for each state) rather than piling conditionals
  into SQL comments.
- Seeding (e.g., `logical_agents` seeded from the catalog in Sprint
  v002-s03-02) happens in app-layer startup code, not in migrations —
  migrations are schema, not state.
