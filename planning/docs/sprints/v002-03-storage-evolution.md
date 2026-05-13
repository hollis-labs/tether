# Sprint v002-03 — Storage Evolution

Epic: [v0.0.2](../epics/v0.0.2-runtime-foundation.md)

**Epic:** [v0.0.2](../epics/v0.0.2-runtime-foundation.md)
**Goal:** Evolve the SQLite schema to reflect the context-pack entity model. Introduce a migration framework. Split durable LogicalAgent identity from ephemeral RuntimeSession. Add tables for Checkpoint, BrokerEnvelope, Events, and ClientAttachment. Keep the existing demo-launch smoke path working throughout.
**Exit criteria:**
- [x] A migration framework is in place; `v0_0_1 → v0_0_2` migration runs on first daemon start and is idempotent. (Sprint v002-02 pull-forward, migration 0001–0006.)
- [x] `logical_agents`, `checkpoints`, `broker_envelopes`, `events` (evolved), `client_attachments` tables exist with the fields described in context-pack §02.
- [x] `sessions` table has `logical_agent_id` referencing `logical_agents.id`; existing YAML catalog agents are seeded as logical agents on first run. (Clean rename rather than add-alongside per ADR 0003.)
- [x] Store API in `internal/store/` exposes CRUD for each new entity behind small per-entity files (no god-struct). One file per entity: `logical_agents.go`, `checkpoints.go`, `broker_envelopes.go`, `client_attachments.go`; `LogEvent` stays in `sqlite.go` as a thin helper.
- [x] `mux` CLI + existing tests still pass end-to-end. `make test-race` green across 12 packages; smoke verified with api-stub-launch.

## Context

Context-pack §02 defines the entity model (LogicalAgent, RuntimeSession, Checkpoint, BrokerEnvelope, CapabilityAsset). Context-pack §05 storage guidance lists the tables explicitly: `logical_agents, runtime_sessions, checkpoints, broker_envelopes, artifacts, event_log, client_attachments` — "Keep schema evolvable. Use migrations."

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/schema.sql` — has `sessions`, `launch_plans`, `events`. No logical agents, no checkpoints, no broker. `events` is a basic session-scoped append log.
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/sqlite.go` — all CRUD in one file; `modernc.org/sqlite` pure-Go driver.
- Schema is applied as a single blob on startup (`db.Exec(schema)`). No migration tracking.

When done, the storage layer reflects the future entity model and is safe to evolve without rewriting.

## Tasks

### T-v002-s03-01: Pick a migration framework and convert existing schema

**kind:** decision + agent
**priority:** 1
**manual:** true
**status:** done (pulled forward into Sprint v002-02 — `feat/v002-s02-live-attach-input`)
**tags:** [decision, storage, migrations]

#### Problem

`internal/store/sqlite.go` executes the full `schema.sql` as a single blob on every startup with `CREATE TABLE IF NOT EXISTS`. Incremental schema changes (this sprint's reason for existing) can't happen safely that way.

#### Fix direction

Pick one:
- **Option A:** embedded `.sql` files per version + a small hand-rolled migrator tracking applied versions in a `schema_migrations` table. Minimal dependency, full control. Recommended for v0.0.2 given the small scale.
- **Option B:** `github.com/pressly/goose` — mature, Go-native, works with `modernc.org/sqlite`.
- **Option C:** `github.com/golang-migrate/migrate` — broader ecosystem, heavier.

Recommend Option A unless readiness review wants ecosystem compatibility now. Document the choice as an ADR (see Sprint v002-07).

Convert the existing `schema.sql` into `migrations/0001_init.sql` and apply via the new migrator. On first daemon start against an existing v0.0.1 database, the migrator should stamp `0001` without re-running.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/` (new; embedded via `//go:embed`)
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrator.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/sqlite.go` — call migrator instead of applying blob schema
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/` (new; ADR for the choice)

#### Acceptance criteria

- [x] Fresh DB: migrator creates `schema_migrations` and applies `0001_init`.
- [x] Existing v0.0.1 DB: migrator detects existing tables, marks `0001` applied without rerunning, logs a clear message.
- [x] Tests cover both paths.
- [x] Choice documented as an ADR. → `docs/adr/0001-migration-framework.md`

#### Test plan

- Unit: fresh in-memory DB → migrate → assert tables + `schema_migrations` row.
- Unit: pre-populated DB (existing schema) → migrate → assert no duplicate execution, assert `schema_migrations` seeded.
- Manual: run against a real v0.0.1 DB file, verify no data loss.

#### Scope fences

- Do not add new tables in this task. This is the framework only.
- Do not add migration *rollback* logic — forward-only is acceptable for v0.0.2.
- Do not switch away from `modernc.org/sqlite`.

#### Relationship

Blocks: every other task in this sprint and all later sprints that touch schema.

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) ("Keep schema evolvable. Use migrations.").

---

### T-v002-s03-02: Add `logical_agents` table and split from `sessions`

**kind:** agent
**priority:** 1
**manual:** true
**status:** done (`feat/v002-s03-storage-evolution`, see ADR 0003)
**tags:** [feature, storage, logical-agent]

#### Problem

v0.0.1 `sessions.agent_id` is a free-form string (the YAML catalog ID). There's no durable logical-agent record, no place to attach policies, and no way to share an identity across multiple runtime sessions.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/internal/store/schema.sql` defines `sessions` with `agent_id TEXT NOT NULL`, no foreign key, no separate table.

#### Fix direction

Migration `0002_logical_agents.sql`:
- `CREATE TABLE logical_agents (id TEXT PRIMARY KEY, role TEXT, name TEXT, responsibilities TEXT, capabilities TEXT, memory_scopes TEXT, policies_json TEXT, permitted_tools TEXT, escalation_rules TEXT, checkpoint_policy TEXT, hot_cold_policy TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);`
- Add `sessions.logical_agent_id TEXT REFERENCES logical_agents(id)`.
- Seed: on daemon start, for every catalog agent ID referenced by a launch, upsert a `logical_agents` row keyed by the catalog ID. This keeps v0.0.1 demo-launch working.
- Update `launch.Plan` to carry `LogicalAgentID` alongside the legacy `AgentID`.

Keep `sessions.agent_id` for one release as a back-compat shim; mark deprecated.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/0002_logical_agents.sql`
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/logical_agents.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/agent/` (new package for the model — or keep as a subpackage of `internal/store` per context-pack §07 `internal/agent`)
- `/Users/chrispian/Projects-apps/agent-mux/internal/launch/resolver.go` — emit `LogicalAgentID`
- `/Users/chrispian/Projects-apps/agent-mux/internal/app/service.go` — seed logical agents on startup

#### Acceptance criteria

- [x] Migration creates `logical_agents` and adds `logical_agent_id` column. → `migrations/0003_logical_agents.sql`
- [x] First daemon start after migration seeds a logical agent for every catalog agent currently referenced. → `seedLogicalAgents` in `internal/app/service.go` (seeds all catalog agents, strict superset)
- [x] New sessions reference the seeded logical agent. → verified end-to-end via smoke: `sqlite3 … "SELECT id, logical_agent_id FROM sessions"`
- [x] Existing demo-launch still works end-to-end. → `internal/launch/resolver_test.go::TestResolveDemoLaunch` asserts `plan.LogicalAgentID == "demo-agent"`; `make test-race` green across 12 packages

**Deviation from Fix direction**: sprint said "keep `sessions.agent_id` as back-compat shim, deprecate only". Implementation did a clean rename (`agent_id` → `logical_agent_id`, no shim) per memory `feedback_no_compat_shims` (pre-launch, no consumers). Captured in ADR 0003.

#### Test plan

- Unit: resolver produces a plan with non-empty `LogicalAgentID`.
- Integration: run migration against a v0.0.1 DB, verify seeding.
- Manual: `mux launch --launch demo-launch`, `mux sessions get <id>` shows a logical agent ID.

#### Scope fences

- Do not pre-populate policy fields beyond the catalog can express. Leave them null for v0.0.2.
- ~~Do not remove the legacy `agent_id` column in this task — deprecate only.~~ Reversed at readiness pass; see ADR 0003.
- Do not introduce `logical_agents` admin CLI here (warm/cool, policy edits). That's v0.1.

#### Relationship

Depends on: T-v002-s03-01.
Blocks: T-v002-s03-03 (checkpoints reference logical_agents).

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) LogicalAgent entity, [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md).

---

### T-v002-s03-03: Add `checkpoints` table and model

**kind:** agent
**priority:** 2
**manual:** true
**status:** done (`feat/v002-s03-storage-evolution`)
**tags:** [feature, storage, checkpoint]

#### Problem

No way to record a durable continuity record for a logical agent. Without the table, v0.0.3's checkpoint-resume path has nothing to land against.

#### Fix direction

Migration `0004_checkpoints.sql` (note: `0003` was consumed by T-02 logical_agents):
- `CREATE TABLE checkpoints (id TEXT PRIMARY KEY, logical_agent_id TEXT NOT NULL REFERENCES logical_agents(id), task_id TEXT, workflow_id TEXT, status TEXT, completed_work TEXT, pending_work TEXT, key_decisions TEXT, referenced_artifacts TEXT, summary TEXT, next_recommendation TEXT, created_at TEXT NOT NULL, source_session_id TEXT REFERENCES sessions(id));`
- CRUD in `internal/store/checkpoints.go`.
- Light Go types in `internal/checkpoint/model.go` per context-pack §07.

No business logic (create/resume flows) in this task — just storage primitives and a pure `internal/checkpoint` package with types.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/0004_checkpoints.sql`
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/checkpoints.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/checkpoint/` (new package; types only)

#### Acceptance criteria

- [x] Migration creates the table. → `migrations/0004_checkpoints.sql` + 2 indexes
- [x] CRUD works (insert, get, list-by-logical-agent). → `internal/store/checkpoints.go`
- [x] `internal/checkpoint` exposes a `Checkpoint` struct mirroring the columns.

#### Test plan

- [x] Unit: insert + list + get round-trip. → `checkpoints_test.go::TestCheckpoints_CreateGetList`
- [x] Unit: application-level validation that required fields (id, logical_agent_id, created_at) are rejected. Note: SQL-level FK violation when logical_agent_id references a nonexistent agent is not enforced in v0.0.2 because `PRAGMA foreign_keys` is off per ADR 0003; deferred FK-enforcement ADR will add that later.

#### Scope fences

- Do not implement create-from-session or resume-to-session logic. That's Sprint v003-04.
- Do not add checkpoint API endpoints here (Sprint v002-05).

#### Relationship

Depends on: T-v002-s03-02.

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) Checkpoint entity.

---

### T-v002-s03-04: Add `broker_envelopes` table and model

**kind:** agent
**priority:** 2
**manual:** true
**status:** done (`feat/v002-s03-storage-evolution`)
**tags:** [feature, storage, broker]

#### Problem

No storage for inter-session messages. Sprint v002-05 needs to expose broker endpoints that persist; those endpoints need a table to write into.

#### Fix direction

Migration `0005_broker_envelopes.sql` (note: 0004 was consumed by T-03 checkpoints):
- `CREATE TABLE broker_envelopes (id TEXT PRIMARY KEY, sender TEXT, recipient TEXT, workflow_id TEXT, correlation_id TEXT, message_type TEXT, priority INTEGER, payload TEXT, created_at TEXT NOT NULL, delivered_at TEXT, consumed_at TEXT, audit_json TEXT);`
- Indexes: `(recipient, delivered_at)`, `(workflow_id, correlation_id)`.
- CRUD in `internal/store/broker_envelopes.go`.
- Types in `internal/broker/model.go`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/0005_broker_envelopes.sql`
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/broker_envelopes.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/broker/` (new package; types only in this sprint)

#### Acceptance criteria

- [x] Migration creates table + indexes. → `migrations/0005_broker_envelopes.sql` (2 indexes: recipient+delivered_at, workflow+correlation)
- [x] CRUD works (put, get, list-by-recipient, list-by-workflow). → `internal/store/broker_envelopes.go` (`CreateEnvelope`, `GetEnvelope`, `ListEnvelopesByRecipient`, `ListEnvelopesByWorkflow`)
- [x] `internal/broker` exposes an `Envelope` struct.

#### Test plan

- [x] Unit: round-trip envelope insert/get. → `TestEnvelopes_RoundTripAndRecipient`
- [x] Unit: correlation lookup returns request+response envelopes in order. → `TestEnvelopes_CorrelationLookup`

#### Scope fences

- Do not implement request/reply semantics here — that's Sprint v003-05.
- Do not implement routing logic — that's Sprint v002-05 (API) + v003-05 (semantics).

#### Relationship

Depends on: T-v002-s03-01.
Blocks: T-v002-s05-04 (broker endpoints), T-v003-05-01 (request/reply).

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) BrokerEnvelope entity, [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) broker operations.

---

### T-v002-s03-05: Evolve `events` table; add `client_attachments`

**kind:** agent
**priority:** 3
**manual:** true
**status:** done (`feat/v002-s03-storage-evolution`). `client_attachments` was pulled forward into Sprint v002-02 (migration 0002); this task shipped only the events evolution.
**tags:** [feature, storage, events, observability]

#### Problem

Current `events` is session-scoped (`session_id NOT NULL`). The runtime event bus (Sprint v002-06) needs to publish non-session events (daemon lifecycle, broker events). `client_attachments` is needed by Sprint v002-02.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/internal/store/schema.sql`:
```sql
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    ...
```

#### Fix direction

Migration `0006_events_evolution.sql` (events-only; 0005 was consumed by T-04 broker_envelopes, and `client_attachments` already exists from migration 0002):
- Rebuild `events` with nullable `session_id`, `scope TEXT NOT NULL DEFAULT 'session'`, and `payload_json TEXT` (replacing `payload`). Clean rebuild — no legacy `payload` column survives per no-compat-shims policy (LogEvent had no consumers; verified by grep).
- `stream_seq` was **not** added. The existing `id INTEGER PRIMARY KEY AUTOINCREMENT` provides monotonic stream ordering via sqlite_sequence and a separate counter would duplicate that guarantee. Revisit in Sprint v002-06 if a per-scope counter is actually needed.
- Backfill `scope='session'` for existing rows via `INSERT INTO events_new (…, scope, …) SELECT …, 'session', …`.

Update `store.LogEvent(scope, sessionID, kind, payloadJSON)` — scope required; empty sessionID ↔ SQL NULL. Ctx not added; matches existing store method signatures.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/0006_events_evolution.sql`
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/sqlite.go` — `LogEvent` signature + body updated. `client_attachments` CRUD already exists (`internal/store/client_attachments.go`, Sprint v002-02).
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/` (new package; types only here, bus in Sprint v002-06)

#### Acceptance criteria

- [x] Migration runs without losing data. → `migrator_test.go::TestMigrate_0006_EventsBackfill` seeds v0.0.1 events, migrates, verifies `scope='session'` + `payload_json` carried across; legacy `payload` column dropped.
- [x] `LogEvent(scope, sessionID, kind, payloadJSON)` signature available (ctx omitted to match the existing store method shape; no code depends on it).
- [x] `client_attachments` CRUD works. → already landed in Sprint v002-02.
- [x] Existing session-event writes continue to work. → `LogEvent(events.ScopeSession, …)` is the path; no caller existed pre-T-05, so no shim was needed.

#### Test plan

- [x] Unit: pre-populate v0.0.1-style events, migrate, assert `scope='session'` + payload_json carried across. → `TestMigrate_0006_EventsBackfill`.
- [x] Unit: write a non-session event (scope='daemon', session_id=null), verify persisted. → `events_test.go::TestLogEvent_SessionAndDaemonScopes`.

#### Scope fences

- Do not implement the pub/sub bus here — that's Sprint v002-06.
- Do not build an events query API (Sprint v002-05/v002-06).
- Do not restructure the `events` payload format beyond adding `payload_json` — free-form JSON is fine.

#### Relationship

Depends on: T-v002-s03-01.
Pairs with: T-v002-s02-03 (client_attachments writer), T-v002-s06-01 (event bus).

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) storage list.

## Review / readiness notes

- **Seeding strategy for `logical_agents` from the existing YAML catalog** needs an explicit decision: seed on every daemon start, or only on first migration? Lean toward on-start-upsert so new catalog entries automatically appear. Capture as an ADR.
- **Deprecation path for `sessions.agent_id`** — keep it populated for one release; remove in v0.0.3 or v0.1. Flag if readiness wants a shorter deprecation window.
- **Schema for policies / capabilities** in `logical_agents` is stored as TEXT (probably JSON). This is fine for v0.0.2; don't over-engineer column layouts until v0.1 introduces policy logic.
- **Checkpoint payload schema** is deliberately loose in v0.0.2 (TEXT columns). v0.0.3 will pin it.
