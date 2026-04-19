# internal/store

SQLite-backed persistence for the daemon: sessions, logical agents,
checkpoints, broker envelopes, client attachments, launch plans, events,
and schema migrations.

**Purpose:** single source of durable state. Pure Go driver
(`modernc.org/sqlite`) — no CGO. Multi-reader: the daemon holds the
writer; CLI fallbacks open the DB read-only when the daemon is down.

**Entry points:**

- `Open(path)` (`sqlite.go`) — creates parent dir, opens the DB, runs
  pending migrations, returns `*Store`.
- Per-entity CRUD files:
  - `sqlite.go` — sessions + launch_plans.
  - `logical_agents.go` — upsert-on-seed + list.
  - `checkpoints.go` — create / get / list by agent.
  - `broker_envelopes.go` — create / get / per-recipient FIFO /
    per-workflow + optional correlation filter.
  - `client_attachments.go` — create / detach / list / sweep-stale.
  - `events.go` — log + range read; feeds `events.Persister`.
- `migrator.go` + `migrations/*.sql` — embedded migrations with a
  hand-rolled versioning table. See [ADR 0001](../../docs/adr/0001-migration-framework.md).

**Neighbors:**

- Downstream consumers: [`internal/app`](../app) at startup;
  [`internal/runtime`](../runtime) writes session state transitions;
  [`internal/events`](../events) uses Store as its `Persister`;
  [`internal/api`](../api) reads via service adapters.

**Gotchas:**

- **Rebuild the binary after touching `migrations/`.** Migrations are
  `//go:embed`'d — a stale binary silently skips new versions, and
  later reads fail with "no such column". Always `make build` before
  smoking.
- FK enforcement is **off** (`PRAGMA foreign_keys` unchanged).
  Column-level `REFERENCES` declarations are documentation only. A
  future ADR will flip enforcement on with an orphan-delete migration.
- Pre-launch, `sessions.agent_id` was renamed to `logical_agent_id` in
  migration `0003` — no compat shim, per [ADR 0003](../../docs/adr/0003-logical-agents-seeding.md).
- Event ordering comes from the `events` table's `AUTOINCREMENT id`;
  there is intentionally no separate `stream_seq` column.
