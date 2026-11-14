# Tether on Substrate: architecture

Reviewers read this file first. It answers the Substrate adoption guide's section 0 questions for Tether. The answers come from the port plan (`docs/substrate-port-plan.md` on `main`, PR #560), and the file grows slice by slice. It describes `next/`, the new tree that lives beside the old one on the `substrate-port` branch until slice S15 promotes it.

**State at slice S0:**
- **Modules:** the binary boots config, logging, health, the HTTP host, the database and the CLI.
- **Data directory:** it resolves and secures the data directory and refuses an old database.
- **HTTP:** it serves only `/healthz`.
- **Domain code:** none yet.

## Section 0 answers

| Question | Answer |
|---|---|
| Migrated data or fresh start? | **Migrated, all of it.** Tether is in use and holds the fleet's messaging, registry, sessions, MCP gateway state, events and issuer state. A tested one-way `migrate-from` (S12) copies the old database into the new layout, rehearsed on copies. The old database is never opened in place (see "Old-layout guard"). Plan section 7. |
| What is the binary? | `variant: service`, one `tether` binary. `tether serve` is the hub. The operator commands, the stdio MCP forwarder (`tether mcp --forward-daemon`) and `tether mailbox watch` stay subcommands of the same binary, because agents invoke them by name. There is no UI: sysop is not ported (D4). |
| Who issues identity? | **Two trust roots in one process, verified by the one `auth` path.** Tether's own agent-identity issuer (`authissuer`: agent, session, service `svc-`, device and workload tokens, JWKS, deny-list) stays app code, and its `auth/` directory is carried byte for byte. This is decision D1 (a), the declared exception in `conversion-notes.md`, pending Chrispian's ruling through @archie; S3 does not start before it. The `identity` module is selected for Tether's own operator accounts and API keys. It is not in the S0 manifest; S3 adds it. |
| Which routes are public, which need a scope, which authenticate themselves? | **Today:** only `/healthz` (public, served by `httphost`). **From S4:** the client API is Tether's wire protocol, kept at its paths with its ADR 0010 error envelope as declared `httphost` mounts (`Mount{Raw: true}`). Those mounts verify their own callers through the auth module, and every route on them is scoped with Tether's existing `tether.<verb>` scopes (decision D2). Only routes new to the port (operator identity, setup) live under `/api/`. The full mount list is in `conversion-notes.md`; a coverage test fails on an unscoped route or an unnamed mount. |
| Is there background work that must drain on shutdown? | Yes, from later slices: the worker hub's outpost WebSockets (S11), SSE streams (`mailbox watch`, `/events/stream`; S4/S8), event retention as a `scheduler` job (S8), the team loop (S13) and event ingest. **Stop order:** stop accepting connections; close streams through `httphost.Stopping(r)` with a final frame; drain in-flight requests within the grace period; then stop modules in reverse start order. S0 has nothing to drain: `httphost.Serve` returns on SIGINT or SIGTERM and the modules stop (verified with a real process; see the S0 PR). |
| What does the UI need from the framework? | Nothing; there is no UI. |
| Does the app depend on a module or a lib that is not in the BOM? | Not at S0: `go.mod` holds Substrate (`d064f536`, BOM pins) and `modernc.org/sqlite` (the driver the database module uses). Later slices will need app libs such as `mesh/core` (messaging) and `harness/core` (sandbox, runtime events). Each is checked against `bom/bom.yaml` `app_libs` when its slice starts; a missing pin is a Substrate PR first. |

## Layout

```
next/
  cmd/tether/          main.go, config.go (boot hooks), serve.go, substrate.yaml, substrate_wiring.go (generated, committed)
  internal/settings    the tether: config block, validated at start
  internal/datadir     TETHER_DATA_DIR resolution, 0700/0600 discipline
  internal/legacyguard refuses the old database before the database module opens it
  internal/migrate     schema-equality test (S1); migrate-from lands in S12
  migrations/sqlite/   one goose migration set (00001: the squashed schema)
  docs/                this file, conversion-notes.md, carry-ledger.md
```

`next/` is its own Go module (`github.com/hollis-labs/tether/next`) on the Substrate BOM pins. It cannot import the old `internal/` packages: Go forbids it across modules, so the clean break is enforced by the compiler.

## Data directory

`TETHER_DATA_DIR` is required and is the only setting read from the environment. The service descriptor names a new directory beside the old data root. There is no XDG fallback, because the old tree resolves its state through the same XDG application name.

The `AfterConfig` hook does five things:
- refuses a bad `tether:` block;
- refuses a directory that is the old data root, before anything is created or chmod'ed. It runs `Lstat` only, on `state/tether.db`, `catalog/global.yaml`, `run/tetherd.sock` and `run/tetherd.pid`, which exist only under the old root because the catalog and the socket stay there by path. One wrong descriptor value would otherwise start a hub on an empty database beside the old one;
- resolves the directory and creates it `0700` (or tightens it);
- writes the absolute database path into `app.Config` (`database.sqlite.path`, default `tether.db` inside the directory). An absolute override is kept. A relative path that climbs out of the directory is refused.
- refuses a non-SQLite `database.driver`.

The database file is created `0600` just before the database module opens it. SQLite gives its sidecar files the same mode.

Later slices put the carried issuer `auth/` directory, `operator.token` and file state in the same directory. Three things stay outside it, because clients name them by path (plan section 3):
- **the catalog**, which stays at `<data root>/catalog`;
- **the unix socket**, which stays at the old path `<data root>/run/tetherd.sock`;
- **the remote listener address.**

## Old-layout guard

`Before["database"]` runs `legacyguard.Check` on the configured database path. It reads the file read-only and never modifies it.

**Refused:**

| What is found | Error |
|---|---|
| The old migrator's `schema_migrations` table and no migration mark | `ErrLegacyLayout` |
| A `tether_migration_mark` whose state is not `complete` (an interrupted `migrate-from`) | `ErrIncompleteMigration` |

**Accepted:**
- a missing file;
- an empty file;
- a database the new build created;
- a migrated database with state `complete`.

The mark table belongs to S12 and is specified in `conversion-notes.md`.

## Settings (`tether:` block)

Defaults live in `cmd/tether/substrate.yaml`. Operators override any key in the YAML file named by `SUBSTRATE_CONFIG`. An unknown key, a wrong type or an out-of-range value stops the start with an error naming the key.

| Key | Default | Rule |
|---|---|---|
| `tether.catalog_dir` | `""` (unset until the catalog slice) | absolute path when set |

Event retention is not a key yet. S8 adds it with the old `daemon.events_retention` semantics and defaults: `enabled` true, `days` 90, `debug_days` 7, and 0 or negative disables. A test pins those defaults to the old ones, so a default can never narrow the live 90-day window that the migrated `events` rows sit in.

## Database

SQLite only, through the `database` module (`database.From(app)`: one writer, a read pool), because Tether's SQL uses `?` placeholders and SQLite syntax.

There is one embedded goose migration set, `migrations/sqlite/`, registered with `database.UseMigrationFS`. It has no `postgres/` directory because the app is SQLite only.

### The squashed schema (`00001_schema.sql`)

`00001_schema.sql` is the old store's schema at its migration 0101 (`main` at `f6c37bfa`), in one goose migration. It is generated, never hand-edited. `internal/migrate/schema_test.go` rebuilds it from the old migrations, which are vendored under `internal/migrate/testdata/oldmigrations`:

```sh
go test ./internal/migrate -run TestSchemaEqualsOld -update   # regenerate, then review the diff
```

The same test proves the two equal on every run:
- It applies the vendored migrations the way the old migrator did (each file as one Exec in its own transaction, in version order).
- It applies `00001` through the go-store/goose path the database module uses.
- It compares the two databases object by object: the normalized `CREATE` text, every column (type, NOT NULL, default, key position), every index (columns, uniqueness, origin, partial flag), every foreign key, every trigger, and the rows a fresh database holds (`message_event_checkpoints` is seeded with `delivery` -1, `attention` 0 and `retry` 0).

Every difference must be on the test's allowlist:

| Object | Where it goes | Why |
|---|---|---|
| `ai_events` and its four indexes | archived to `llm-archive.db` by `migrate-from` (S12) | plan D3: the LLM gateway is out of scope |
| `schema_migrations` | replaced by `goose_db_version`; the old rows are kept in the archive as provenance | the old migrator's ledger |
| `goose_db_version` (new only) | | the database module's version table |

Three things are outside this test by design:
- **The seven `messaging_*` tables** (`messaging_messages`, `_deliveries`, `_attempts`, `_receipts`, `_transitions`, `_idempotency`, `_delivery_schema`) belong to go-messaging (`libs/mesh/core`). The old store has `delivery.ApplySQLiteSchema` create them at open; the port does the same, so their DDL is not duplicated here. `TestNoLibraryTables` keeps them out of `00001`. The library version is pinned when the messaging slice imports it (see the Gap in `conversion-notes.md`).
- **Three triggers created in Go at open**, by the old `internal/store/session_replacement.go`: `replaced_delivery_claim`, `replaced_delivery_receipt` and `replaced_shim_delete`. The ported store code recreates them (S2); `migrate-from` does not copy them.
- **Transitional `*_new` / `*_v2` / `*_verified` / `*_upgrade` tables.** None survives the old migrations: each is renamed or dropped by a later one (`TestNoLeftovers`). So the squashed schema drops no table that holds rows.

Rules for row identity follow from the schema and `migrate-from` keeps them:
- Every INTEGER PRIMARY KEY and AUTOINCREMENT is exactly as before, so ids and `sqlite_sequence` carry over.
- `messages.rowid` is the mailbox watch cursor clients hold, so `migrate-from` copies it explicitly.
- Foreign keys are declared as before and stay unenforced (ADR 0008).

A schema change made on old `main` after `f6c37bfa` (for example `0102_worker_retirement.sql`) reaches the port through the carry ledger as a new migration, `00002` onwards, plus its `migrate-from` transform. It is never an edit to `00001`.

### Tables

108 tables: the 117 in the old store minus `ai_events`, `schema_migrations` and the seven library tables. FTS5 shadow tables and `sqlite_sequence` are created by SQLite and not listed.

| table | primary key | key is the rowid | AUTOINCREMENT | triggers |
|---|---|---|---|---|
| a2a_tasks | binding_id, task_id | no (implicit rowid) |  |  |
| agent_recovery_cursors | agent_id, channel | no (implicit rowid) |  |  |
| broker_envelopes | id | no (implicit rowid) |  |  |
| channel_publications | seq | yes | yes |  |
| checkpoints | id | no (implicit rowid) |  |  |
| client_attachments | id | no (implicit rowid) |  |  |
| codex_shim_protocol | session_id | no (implicit rowid) |  | 3 |
| environment_request_resolutions | session_id, turn_id, request_id | no (implicit rowid) |  |  |
| environment_request_status | session_id, turn_id, request_id | no (implicit rowid) |  |  |
| environment_request_turn_ends | session_id, turn_id | no (implicit rowid) |  |  |
| environment_work_state | session_id | no (implicit rowid) |  |  |
| event_identity | singleton | yes |  |  |
| event_ingest_digests | event_id | no (implicit rowid) |  |  |
| event_retention_admission | kind | no (implicit rowid) |  |  |
| event_retention_policies | id | no (implicit rowid) |  | 2 |
| events | id | yes | yes | 1 |
| execution_policy | id | yes |  |  |
| fabric_actors | record_key | no (implicit rowid) |  |  |
| fabric_admissions | record_key | no (implicit rowid) |  |  |
| fabric_agents | record_key | no (implicit rowid) |  |  |
| fabric_binding_heads | record_key | no (implicit rowid) |  |  |
| fabric_binding_history | record_key | no (implicit rowid) |  |  |
| fabric_definition_artifacts | record_key | no (implicit rowid) |  |  |
| fabric_definition_revisions | record_key | no (implicit rowid) |  |  |
| fabric_import_receipts | record_key | no (implicit rowid) |  |  |
| fabric_instances | record_key | no (implicit rowid) |  |  |
| fabric_legacy_refs | record_key | no (implicit rowid) |  |  |
| fabric_migration_candidates | record_key | no (implicit rowid) |  |  |
| fabric_outbox | cursor | yes | yes |  |
| fabric_sessions | record_key | no (implicit rowid) |  |  |
| forum_categories | id | no (implicit rowid) |  |  |
| forum_participants | forum_id, principal_kind, principal_id | no (implicit rowid) |  |  |
| forum_post_fts | none (FTS5) | - | - | 0 |
| forum_posts | id | no (implicit rowid) |  |  |
| forum_topics | id | no (implicit rowid) |  |  |
| forums | id | no (implicit rowid) |  |  |
| group_members | grp_urn, member_urn | no (implicit rowid) |  | 1 |
| identity_audit | id | yes | yes |  |
| launch_plans | session_id | no (implicit rowid) |  | 2 |
| logical_agents | id | no (implicit rowid) |  |  |
| message_aliases | urn | no (implicit rowid) |  |  |
| message_event_checkpoints | source | no (implicit rowid) |  |  |
| message_event_deferred | transition_id | yes |  |  |
| message_event_journal | id | yes | yes |  |
| message_purge_audit | id | yes | yes |  |
| messages | id | no (implicit rowid) |  | 8 |
| pairing_grants | id | no (implicit rowid) |  |  |
| principals | id | yes | yes | 2 |
| proxy_events | id | yes | yes |  |
| recipient_wake_admissions | message_id | no (implicit rowid) |  |  |
| registry_capabilities | urn, capability | no (implicit rowid) |  |  |
| registry_entries | urn | no (implicit rowid) |  | 3 |
| registry_external_ids | urn, substrate | no (implicit rowid) |  |  |
| registry_links | urn, kind, target | no (implicit rowid) |  |  |
| registry_skills | urn, name | no (implicit rowid) |  |  |
| retention_audit | id | yes | yes |  |
| routing_replies | reply_id | no (implicit rowid) |  |  |
| runtime_bindings | id | no (implicit rowid) |  |  |
| scoped_role_bindings | id | no (implicit rowid) |  |  |
| seat_status | agent_urn | no (implicit rowid) |  |  |
| seat_wake_clock | seat | no (implicit rowid) |  |  |
| session_groups | id | no (implicit rowid) |  |  |
| session_idempotency | principal_id, key | no (implicit rowid) |  |  |
| session_mcp_policy | session_id | no (implicit rowid) |  |  |
| session_provider_mappings | session_id, owner, provider | no (implicit rowid) |  | 3 |
| session_refs | id | yes |  |  |
| session_replacements | source_session_id | no (implicit rowid) |  | 2 |
| session_shim_retention_cursors | cursor_id | no (implicit rowid) |  |  |
| session_shim_retirements | operation_id | no (implicit rowid) |  |  |
| session_shims | session_id | no (implicit rowid) |  | 2 |
| session_usage | session_id | no (implicit rowid) |  |  |
| session_worker_seq | session_id | no (implicit rowid) |  |  |
| sessions | id | no (implicit rowid) |  | 6 |
| settings | scope, scope_id, key | no (implicit rowid) |  |  |
| team_activation_evidence | kind, evidence_key | no (implicit rowid) |  |  |
| team_definitions | team_id, version | no (implicit rowid) |  |  |
| team_gone_shim_recoveries | shim_key | no (implicit rowid) |  |  |
| team_host_adoptions | intent_key | no (implicit rowid) |  |  |
| team_host_approvals | request_key | no (implicit rowid) |  |  |
| team_host_bindings | actor | no (implicit rowid) |  |  |
| team_host_calls | principal, verb, call_key | no (implicit rowid) |  |  |
| team_host_delegations | delivery_key | no (implicit rowid) |  |  |
| team_host_deliveries | sequence | yes |  |  |
| team_host_intents | sequence | yes |  |  |
| team_host_launch_intents | intent_key | no (implicit rowid) |  |  |
| team_host_messages | message_key | no (implicit rowid) |  |  |
| team_host_recovery_cursors | kind | no (implicit rowid) |  |  |
| team_host_routing | run_id | no (implicit rowid) |  |  |
| team_host_workflows | launch_key | no (implicit rowid) |  |  |
| team_launch_grants | intent_key | no (implicit rowid) |  |  |
| team_launch_journal | launch_key, revision | no (implicit rowid) |  |  |
| team_launch_leases | launch_key | no (implicit rowid) |  |  |
| team_launches | launch_key | no (implicit rowid) |  |  |
| team_phase_signals | sequence | yes | yes |  |
| team_port_intents | sequence | yes | yes |  |
| team_recovery_attempts | kind, intent_key | no (implicit rowid) |  |  |
| team_roster_snapshots | run_id, version | no (implicit rowid) |  |  |
| team_rosters | run_id | no (implicit rowid) |  |  |
| team_runs | run_id | no (implicit rowid) |  |  |
| team_signal_resolutions | run_id, phase_id | no (implicit rowid) |  |  |
| turn_inputs | input_id | no (implicit rowid) |  |  |
| wake_triage | message_id | no (implicit rowid) |  |  |
| worker_mail_grants | worker_id, urn | no (implicit rowid) |  |  |
| worker_permission_parser | session_id | no (implicit rowid) |  |  |
| workers | id | no (implicit rowid) |  |  |
| workspace_scratch_allocations | session_id | no (implicit rowid) |  |  |
| workstream_projects | workstream_id, project_urn | no (implicit rowid) |  |  |
| workstreams | id | no (implicit rowid) |  |  |


## HTTP

At S0, `httphost` binds `127.0.0.1:8949` (loopback; a port neither old listener uses) and serves `/healthz`:

```json
{"status":"ok","checks":{"database":"ok"}}
```

S4 replaces this with two listeners:
- the unix socket at its old path;
- the remote listener at its old loopback address, with the closed client mount list.
