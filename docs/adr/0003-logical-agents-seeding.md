# ADR 0003: LogicalAgent Seeding and Clean `agent_id` Rename

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-03 (Storage Evolution), task T-v002-s03-02
**Deciders:** agent-mux v0.0.2 execution session

## Context

Context-pack §02 defines `LogicalAgent` as the durable-identity entity
(id, role, responsibilities, capabilities, policies, checkpoint policy,
hot/cold policy, …). v0.0.1 stored only a free-form `sessions.agent_id
TEXT NOT NULL` holding the YAML catalog agent ID (e.g. `claude-code`),
with no dedicated table and no place to attach policy or identity
metadata.

Two questions this ADR resolves:

1. **Where does `logical_agents` data come from at runtime?** Migrations
   create the table but leave it empty. The catalog already knows which
   agents exist. Seed path: (a) one-shot at migration time, (b) on every
   daemon start, or (c) explicit admin command.
2. **What happens to `sessions.agent_id`?** The sprint plan suggested a
   back-compat coexistence (`agent_id` stays, `logical_agent_id` added
   alongside, `agent_id` deprecated for one release). Portfolio policy
   (`feedback_no_compat_shims`) says pre-launch, don't carry shims.

## Decision

### 1. Seed `logical_agents` on every daemon start via upsert from catalog

After the store is opened and migrations have run, `app.Service.New`
walks every `launch.Agent` referenced by a catalog `Launch` and does an
upsert into `logical_agents` keyed by the catalog agent ID. The upsert
populates `id`, `role` (from `catalog.Agent.Roles` joined), `name`, and
refreshes `updated_at`; `created_at` is preserved if the row already
exists. Policy / checkpoint-policy / hot-cold columns stay `NULL` for
v0.0.2 — the catalog can't express them yet.

Rationale:

- **New catalog entries auto-appear** without an operator running a
  seed command. Matches §05 "Keep schema evolvable" + §04 use-cases
  where catalogs churn.
- **Seeding lives in app code, not migrations.** Migrations are schema
  transforms; populating from external sources (the catalog) is state
  and belongs in startup. (See ADR 0001, closing note.)
- **Upsert, not truncate-and-replace.** Preserves any future
  operator-set fields (manual policy edits in v0.1) and keeps
  `created_at` stable across restarts.

### 2. Clean-rename `sessions.agent_id` → `sessions.logical_agent_id`

`0003_logical_agents.sql` performs the full table-rebuild dance:

1. `CREATE TABLE logical_agents (…)` with the full §02 column set.
2. `INSERT OR IGNORE INTO logical_agents (id, name, created_at,
   updated_at) SELECT DISTINCT agent_id, agent_id, <now>, <now> FROM
   sessions` — backfills synthetic logical-agent rows for any v0.0.1
   session whose agent_id hasn't been seeded yet.
3. `CREATE TABLE sessions_new (…, logical_agent_id TEXT NOT NULL
   REFERENCES logical_agents(id), …)` — same shape as `sessions`
   except the column is renamed and made an FK.
4. `INSERT INTO sessions_new SELECT …, agent_id AS logical_agent_id,
   … FROM sessions;` — carry the existing value.
5. `DROP TABLE sessions;` + `ALTER TABLE sessions_new RENAME TO
   sessions;` + recreate `idx_sessions_state`.

No `agent_id` column survives. No coexistence shim. No deprecation log.

Rationale:

- **Pre-launch**, catalog agent ID ≡ logical agent ID. They are the same
  concept with a better name; "shimming" would only persist the
  ambiguity.
- **`feedback_no_compat_shims`** (portfolio memo): when there are no
  consumers, break cleanly. There are no consumers of v0.0.1 agent_id
  outside this repo's own code.
- **`feedback_plan_is_best_effort`** (portfolio memo): sprint plans
  adjust to reality. The sprint suggested `keep agent_id for one
  release`; the reality is we ship pre-launch and there's no downside
  to a clean column rename.
- **Data preservation:** existing sessions rows survive the rebuild with
  their identifier intact (just relocated into `logical_agent_id`). The
  pre-backfill step ensures the new FK resolves for historical rows.
- If v0.1 later needs to separate "catalog agent that launched this
  session" from "current logical identity", that's a new column
  (`catalog_agent_id` or `launched_as`), not a revival of the old one.

### 3. FK enforcement stays off

`PRAGMA foreign_keys` is **not** enabled. SQLite parses `REFERENCES`
declarations but does not enforce them without the pragma. Current
v0.0.1 behaviour keeps FKs advisory; flipping enforcement on has a
non-trivial blast radius (every delete path, every test that pre-seeds
a subset of tables). Deferred to a later ADR (likely Sprint v002-07 or
v003 when the broker/checkpoint fans out).

## Consequences

**Positive**

- One column for one concept. `sessions.logical_agent_id` is the only
  identifier of the agent that owns the session.
- Catalog remains the source of truth for agent definitions; the DB
  reflects catalog state at the moment of last daemon start.
- Future sprints (checkpoints, broker, hot/cold) can reference
  `logical_agents(id)` directly.

**Negative**

- Any external tooling that scraped `sessions.agent_id` from the SQLite
  file breaks. Acceptable: no such tooling exists today.
- Seed-on-start adds a small O(agents) startup cost; negligible at
  current scale.
- Soft FK means a mis-seeded DB could have dangling
  `sessions.logical_agent_id` values; daemon-side seed-before-launch
  keeps this invariant for any session the daemon creates.

## Notes for future ADRs

- If/when policy/checkpoint/hot-cold columns grow schema (Sprint v003
  or later), the upsert will start merging specific columns only —
  don't let seeding overwrite operator-provided values. Flag this as
  soon as the catalog gains policy syntax.
- When FK enforcement lands, include a one-time migration that
  `DELETE`s sessions whose `logical_agent_id` no longer resolves (data
  cleanup) before flipping the pragma.
