# Tether conversion notes

What changed between the old tree and `next/`, and every place the port departs from the Substrate defaults on purpose. This file is updated by every slice. The decisions D1 to D6 are in the port plan (`docs/substrate-port-plan.md` on `main`, section 0).

## Gaps (declared exceptions to the Substrate defaults)

### D1: the agent-identity issuer stays app code

**Status:** recommended (a); Chrispian's ruling through @archie is pending. S3 does not start before it.

Tether is the fleet's token issuer: agent and session tokens, exchange tokens, non-expiring service tokens on a separate `svc-` key, device pairing, and workload trust. Other apps verify against its JWKS and poll its deny-list. Substrate's `identity` module issues a different key format and has no non-expiring service tokens, so the issuer is kept as app code.

**Conditions (@substrate-manager, 2026-11-14):**
1. This declaration, with the open routes named below.
2. The `auth/` directory carried byte for byte: `keys.json`, `<kid>.key`, `svc-<kid>.key`, `jwks.json`, `state.json`. Checksums before and after, modes kept, old files never written. Rotation is tested across the cutover boundary.
3. Operator accounts and API keys through the `identity` module, with `LocalScopes` excluding any scope an agent must not hold.
4. One verifying path, through the Substrate `auth` module (needs the in-process key-source gap, CW-20261114-0097).
5. No change to the claims or to the issuer URL.

Option (c), adding Tether's token classes to Substrate `identity`, is the later framework task CW-20261114-0098.

**Open routes of the issuer.** These are paths and methods as served today (`internal/api/remote_scopes.go` at `f6c37bfa`), all kept at their paths (D2).

| Route | Methods | Caller check today |
|---|---|---|
| `/.well-known/jwks.json` | GET, HEAD | public |
| `/auth/denylist` | GET, HEAD / POST | public / admin |
| `/auth/pair/exchange` | POST | public (one-time pairing grant) |
| `/auth/token` | POST | self (exchange) |
| `/auth/renew` | POST | self |
| `/auth/workload-trust` | POST | workload trust scope |
| `/auth/pair`, `/auth/pair/revoke` | POST | local socket only |
| `/auth/service-tokens`, `/auth/service-tokens/revoke`, `/auth/service-tokens/rotate-key`, `/auth/service-tokens/request` | GET, POST | local socket only |
| `/auth/devices`, `/auth/keys` | GET | admin |
| `/auth/revoke` | POST | admin |
| `/auth/context` | GET | read |

The JWKS, deny-list and `POST /events` calls are made by third-party programs, so they are exempt from the `Tether-Protocol` header gate (`internal/environment/protocol.go`, `thirdPartyRead`). That exemption is part of the contract.

### D2: the client API is protocol-fixed

**Status:** recommended (b).

The client API keeps its root paths and the ADR 0010 error envelope `{"error":{"code","message"}}`. The routes are declared `httphost` mounts with `Mount{Raw: true}`, so `apierr.Normalize` does not flatten them. Callers are verified through the auth module and every route keeps its `tether.<verb>` scope. Only routes new to the port live under `/api/` with flat errors.

**Mount families.** Taken from the remote route table at `f6c37bfa`; S4 owns the exact list and its coverage test. Golden error bodies, one per family, are in the cell's `golden/mount-errors/`.

| Family | Paths | Main callers (client list) |
|---|---|---|
| issuer | `/auth/...`, `/.well-known/jwks.json` | every JWKS consumer, outposts, operators |
| environment | `/.well-known/tether/environment`, `/v1/environment/report`, `/environment/...` | outposts, Tachyon |
| worker | `/worker/v1/connect`, `/workers...` | outposts (frozen wire) |
| health | `/health` | probes |
| sessions | `/sessions...`, `/logical-agents...`, `/session-groups...` | Torque, Hadron, Tachyon, CLI |
| messaging | `/messages...`, `/broker...`, `/groups...`, `/channels...`, `/chat...`, `/mentions` | Nanite, Tachyon, Tangent, Hadron, mailbox watch |
| forums | `/forums...` | Torque proxy |
| registry | `/registry...` | Nanite, Tachyon |
| events | `/events...` (incl. `POST /events`) | Cerberus, Nanite, Torque, Tachyon |
| catalog | `/catalog...` | Tachyon, operators |
| teams | `/teams...` | MCP team tools |
| workstreams | `/workstreams...` | MCP tools |
| operations | `/status`, `/whoami`, `/routing`, `/execution`, `/capacity`, `/settings`, `/docs`, `/fs`, `/logs`, `/proxy` | Torque (`/capacity`), CLI, MCP tools |
| mcp | `/mcp` (remote, bound agent JWT), `/p/<profile>` | outposts' MCP relay |
| a2a | `/a2a...` | optional A2A clients |
| ai | `/ai/...` | **dropped** (LLM gateway, out of scope). Torque's judge still calls `POST /ai/decide` and must move first (client list, row B-10). |

**Refusals that still answer flat under `Raw`.** The host check, and the guard and `route_not_mapped` where they apply. S4 lists these per client, or shows that no client relies on them.

### go-messaging pin: `libs/mesh/core` v0.10.0 is not in the BOM

**Status:** open. Substrate gap task CW-20261114-0126.

The old store runs go-messaging's `delivery.ApplySQLiteSchema` from `libs/mesh/core` **v0.10.0**. The live `messaging_*` tables were created and evolved by that version. The Substrate BOM pins `libs/mesh/core` at **v0.6.0** under `app_libs`. The port must open those tables with the same library version (or a later one whose schema migration accepts them), so the BOM pin has to move first. Until it does, `next/` does not import the library: S1 keeps the library tables out of the squashed schema and compares nothing against it. The messaging slice adds the import, and a test that applies the pinned library schema on top of `00001` and compares it with the old store's.

### go-messaging is SQLite-only (gap CW-20261115-0013)

**Status:** open. Substrate gap task CW-20261115-0013, routed to the libs owner.

go-messaging's delivery store (`messaging_*` tables, class C) has only a SQLite implementation, and it needs a raw `*sql.DB`. The port reaches it through `Stores.LibraryDB(ctx)` (S2a): the one sanctioned raw handle, SQLite only, refused inside a `WriteTx` or `Joint`. Library-backed code returns `ErrLibraryUnavailable` on Postgres, and its tests skip there with a notice. Class C cannot move to Postgres until the library has a Postgres store.

The Postgres store will also need the two replacement guard triggers the old store created in Go at open, on `messaging_attempts` (`replaced_delivery_claim`) and `messaging_receipts` (`replaced_delivery_receipt`). They cannot be migrations, because the library creates those tables after the migrations run. S2b-3b installs them through `LibraryDB` on SQLite (`InstallReplacementDeliveryFences`); on Postgres that is a no-op today. The third guard, `replaced_shim_delete` on `session_shims`, is migration `00006` on both dialects (numbers allocated by the lead: 00003-00004 for the 0102/0103 carries, 00005 for the journal lock).

### D6: socket admission stays as today

**Status:** recommended (a).

Today `identity.mode` is `observe`:
- **Socket callers** (planted MCP forwarders, `mailbox watch`, the CLI, `go-tether-client` over UDS) are admitted even with no token or a stale one.
- **The remote TCP listener** always requires a device credential.

**The port keeps that per listener:** the socket at Substrate `permissive` (log what enforce would refuse, refuse nothing), the remote listener enforcing. If the auth module cannot hold a different mode per listener, a `substrate-gap` task is filed, and the remote listener's enforcement becomes a tested check inside its mounts, as it is today. S4 proves it.

### Postgres baseline: decisions and open items (S1b)

**Status:** decided (CW-20261114-0175, accepted by the cell lead), including foreign keys off on both dialects.

Decided rules for `migrations/postgres/`:
1. **Timestamps stay text**, `TEXT COLLATE "C"`, in the same format as on SQLite. The Go code, digests and the `migrate-from` copy are then the same on both dialects. A move to `timestamptz` is a later migration, column by column.
2. **No foreign keys are declared on Postgres**, and none are enforced on SQLite. See "Foreign keys" below.
3. **JSON keeps its declared type.** JSON in `TEXT` stays `text`, JSON in `BLOB` becomes `bytea`, and triggers and indexes cast with `tether_json()`. Moving the team tables' BLOB columns to `jsonb` goes with S2's query port.
4. **`INTEGER` becomes `BIGINT` everywhere**, flags included. `REAL` becomes `DOUBLE PRECISION`. A rowid-alias key, with or without `AUTOINCREMENT`, becomes `BIGINT GENERATED BY DEFAULT AS IDENTITY`.
5. **Other constructs.** `UNIQUE COLLATE NOCASE` becomes a unique index on `lower()`. `GLOB` becomes `~`. FTS5 becomes a `tsvector` with a GIN index, using the `simple` configuration. Each trigger becomes a plpgsql function.
6. **`messages.seq` is `00002` in both sets**, so `00001` stays as approved.

**Foreign keys: off on both dialects (decided by the cell lead).** This is a deliberate carry of live behaviour (ADR 0008), not a gap in the port. Enforcement is a post-adoption decision, taken after a cascade audit (CW-20261115-0020). The SQLite hook below goes when go-store and the database module gain the option (CW-20261115-0012).

The old store never turned `foreign_keys` on, so its 48 declared references and their `ON DELETE CASCADE` / `SET NULL` were never enforced. Live data holds rows that would violate them; the counts are in the cell's private inventory. The port keeps that behaviour:
- **SQLite:** `internal/sqlitefk` turns enforcement off on every connection. go-store opens SQLite with `foreign_keys` on and has no option for it. The package registers a modernc connection hook that runs after the DSN pragmas, and the binary imports it. When go-store and the database module gain the option, the package goes (Substrate gap CW-20261115-0012).
- **Postgres:** the baseline declares no foreign keys.
- **Tests:** `TestTriggerParity`'s "foreign keys not enforced" scenario shows both dialects accept an orphan row and never cascade a delete. `internal/sqlitefk` tests the hook against a control without it.
- **Orphan rows are data.** `migrate-from` copies them as they are.

Turning enforcement on later is a separate decision for @archie. It changes delete semantics on live data: deleting a session would then remove its launch plan, MCP policy, bindings, refs, provider mappings, idempotency rows and shim, where today they stay. Such a decision must keep every orphan row (quarantined, never deleted), review every cascade before the delete paths rely on it, and declare the same constraints on both dialects.

**Triggers that reach another class (for the class-move plans).** `TestTriggerCrossings` in `internal/store` lists the five triggers whose bodies reach a table of another class. A trigger cannot reach across databases, so each must be redesigned before either of its classes moves:
- `session_terminal_revokes_credentials`, `session_delete_revokes_credentials`: sessions (C) write principals (A). This is blocker B-5 (S9).
- `session_principal_requires_active_session`: principals (A) read sessions (C).
- `replacement_principal_authority`: principals (A) read replacement, binding, team and registry state (C).
- `events_require_id_after_seed`: events (B) read event_identity (A).

The last three are reads, harmless while all classes share one store, and not in plan section 13.7's blocker list. The class-B plan takes the third and the class-C plan the first two (S3/S9).

Known differences between the dialects, handled in the slices that port the queries:
- **Identity gaps.** An identity value is spent by a refused or rolled-back insert; an `AUTOINCREMENT` value is not. Ids keep their order but can have gaps on Postgres, so nothing may read a gap as a deletion.
- **Inserting into `messages`.** Insert code leaves `seq` out. On Postgres an explicit value is stored as given (a second explicit `0` hits the unique index); on SQLite the trigger replaces `0`. Only `migrate-from` writes `seq`, with the old rowid (S2).
- **Id cursors on Postgres.** Readers page `messages.seq`, `events.id` and `message_event_journal.id` by "greater than the last one seen". A Postgres identity value can commit after a higher one, so each writer takes a transaction-scoped advisory lock before its insert, and ids commit in order: `LockMessageSeq` (messages), `eventstore.LockEventSeq` in `InsertTx` (events), and migration `00005`'s statement-level trigger (the journal, covering the message triggers and `RecordMessageWake`). In one transaction the lock order is messages, then the journal, then events. SQLite has one writer and needs none of this.
- **`seq` after deleting the newest message.** On SQLite, `messages.seq` for a new row is `MAX(seq)+1`, which reuses the value of a deleted newest row, exactly as `rowid` did. A Postgres identity never reuses a value. The mailbox watch keeps its old behaviour on SQLite.
- **Timestamp column types.** modernc returns `TIMESTAMP`-declared columns (`settings.updated_at`, `session_idempotency.created_at`) as `time.Time`; Postgres returns the text. The S2 scan code reads them as text on both.
- **Trigger JSON.** The triggers write `message_event_journal.payload_json`, which becomes the emitted event's payload. Postgres builds it as text, byte for byte what SQLite's `json_object` writes. `TestTriggerParity` compares the bytes, including quoting and control characters.
- **Case-insensitive aliases.** Lookups by alias use `lower(alias) = lower(?)`, which serves both dialects. Today's `COLLATE NOCASE` comparisons do not port as written (S2).
- **Forum search.** Search queries differ per dialect: FTS5 `MATCH` against `tsquery` (S11).
- **Statement time.** `tether_now()` and `DEFAULT CURRENT_TIMESTAMP` use the statement start time, as SQLite's `'now'` does.

Items to settle before class C moves to Postgres (the S2 query port keeps SQLite parity until then):
- **Staged turn outputs with a NUL byte or invalid UTF-8** (S2b-2c, #580). Today's store keeps such a body, and on SQLite the port still does (the cutover runs on SQLite). Postgres's `text` cannot hold either, so on Postgres `StageTurnOutput` refuses them with `ErrStagedBodyUnstorable`. Before class C moves, decide between encoding such bodies and the refusal. Existing staged rows that hold such bytes need a carry plan in `migrate-from` (S12), which cannot copy them as they are into `text`. A JSON body's `\u0000` escape is plain text and is unaffected.

- **Same-second session ties** (S2b-3a). "The agent's newest session" breaks `created_at` ties (one-second resolution) by rowid, insertion order, on SQLite, as the old store did. Postgres has no insertion order, so there ties break by id (random): two sessions of one agent created in the same second may then chain the wrong parent or resume the older conversation. Before class C moves, `sessions` needs an insertion-order column (an identity, as `messages.seq`).

Framework items:
- **Version tables.** Substrate #44 (named stores, one goose version table per store) is not in the pinned Substrate (`fe86d444`). Both sets use the default `goose_db_version` until the pin moves. The class-B and class-C stores need #44's per-store table before they move.
- **Extensions.** The Postgres tests use go-store's `storetest`, which creates the `pg_trgm` and `unaccent` extensions on the test server. The server needs them, as stock Postgres 17 images provide.

## Cutover pre-checks (for the runbook)

Checks to run on the old store before the final copy. Each names the slice that found it.

- **No session in `created` holding an admitted workspace allocation** (S2b-3a, #593). The port computes the reservation digest over the new `SessionRow`. The old digest marshalled the nullable columns as `{"Int64":0,"Valid":false}`-style objects, in a different field order, so no old digest matches, whatever the columns hold. A session whose launch stopped between admission and placement would fail every launch after the cutover and stay `created`. That state counts as live, so it blocks resume and recovery for its agent and counts against the parent's child cap. No workspace data is lost. Drain or stop any such session before the final copy.

## Data directory and the old layout

| Old | New |
|---|---|
| Two roots from the old catalog (`defaults.state_db`, for example `<data root>/state/tether.db`), plus XDG via apppaths name `tether` | `TETHER_DATA_DIR` (required, a new directory beside the old data root); database `tether.db` inside it |
| `<data root>/catalog` | unchanged, same path (clients name it) |
| `<data root>/run/tetherd.sock` | unchanged, same path (S4 configures it explicitly) |
| `<data root>/auth/` (issuer) | carried byte for byte into the new data directory (S3/S12) |

**`TETHER_DATA_DIR` at the old data root is refused.** The check runs in `AfterConfig`, read-only, before the directory is prepared. Markers: `state/tether.db`, `catalog/global.yaml`, `run/tetherd.sock`, `run/tetherd.pid`.

**The old database is never opened in place.** `legacyguard` refuses a `tether.db` that has the old migrator's `schema_migrations` table unless migrate-from has marked it complete.

### Migration mark (written by S12, read by the S0 guard)

`tether migrate-from` (`internal/migrate`) creates its bookkeeping tables in the target itself; the goose migrations never create them. The guard reads an existing `tether_migration_mark` as "this database was migrated", so a fresh database must not have one. All three tables are class A.

```sql
CREATE TABLE tether_migration_mark (       -- one row
  id INTEGER PRIMARY KEY CHECK (id = 1),
  state TEXT NOT NULL,                      -- running, bulk, complete
  phase TEXT NOT NULL,                      -- bulk or final: the last run's phase
  source_sha256 TEXT NOT NULL,              -- the source copy the last run read
  started_at TEXT NOT NULL,
  completed_at TEXT                         -- RFC 3339 UTC, when a run verified
);
CREATE TABLE tether_migration_tables (      -- one row per copied table
  table_name TEXT PRIMARY KEY, phase TEXT NOT NULL, rows BIGINT NOT NULL,
  digest TEXT NOT NULL, source_sha256 TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE tether_migration_triggers (    -- SQLite triggers dropped for the copy, until recreated
  name TEXT PRIMARY KEY, table_name TEXT NOT NULL, sql TEXT NOT NULL
);
```

The mark moves through these states:
- **During a run:** `running`.
- **After a verified bulk run:** `bulk`.
- **After a verified final run:** `complete`, the only state `serve` accepts.
- **On a failed verification:** it stays `running`.

The guard (`legacyguard.Check`) reads only `state`. `tether migrate-from` boots with `legacyguard.CheckMigrationTarget` instead, which admits any mark state, since finishing the migration is the command's job, and still refuses an old-layout database.

**A complete database is never rewritten.** It may have served, and a sync would delete the rows written since and revert the ones updated. `migrate-from` refuses it (`ErrTargetComplete`). `migrate-from --verify` re-checks it against a copy and writes nothing.

**Reruns converge; they never duplicate** (cell lead, refining plan 7.2: "discard and start over" is the fallback). Every run walks each table by primary-key range. It compares a digest of the source rows with a digest of the target rows in the same range; equal ranges are kept, a differing range is deleted and re-copied, and target keys beyond the source's last key are deleted. A rerun after a crash or a bulk phase therefore finishes the copy. A final run over an unchanged copy re-copies nothing.

**What the engine refuses, and how it compares:**
- **The source** must be a copy:
  - the path is resolved through symlinks first;
  - a `-wal`, `-shm` or `-journal` file beside it is refused;
  - use VACUUM INTO for the copy (the nightly backup does, and it is the rule for the live database). A `.backup` copy of a WAL database keeps a WAL-mode header with no `-wal` file; it is accepted, but only with the source check (`full` or `quick`, never `none`), since a raw copy of a WAL database's main file without its `-wal` could miss committed pages that nothing else shows. The source is opened immutable and never checkpointed.
- **The source check:** `PRAGMA integrity_check` runs on the source before the copy (`--source-check full`, the default). `quick` runs `quick_check`, and `none` trusts the check made when the copy was taken. The check used and its result are in the report.
- **`--phase` is required.** A run that leaves it out never marks a database complete by default.
- **NULL keys:** a source row with a NULL primary key is refused up front, naming the table, since it would never fall in a key range.
- **Digests** tag each value by its own storage class, so text and a blob of the same bytes differ. On SQLite every value keeps its class through the copy. A Postgres target forces some conversions (text in a bytea column, bytes in a text column); these are applied to the source row before it is digested, and counted per table and column in the report (`conversions`).
- **Reused rowids:** when a source rowid was freed and reused by a row in an earlier key range, the stale holder in the target is deleted first, and its own range is re-copied.

**Ranges, workers and the window** (S12b):
- **Fixed-width key ranges.** A table keyed by a single integer that is its rowid (`events`, `identity_audit` and the other id tables) is walked in fixed-width key ranges (`--batch` is the width) by parallel workers (`--workers`; the CPUs, at most 8). Fixed boundaries are the same in every snapshot, so an insert or a retention delete changes only the ranges that hold it.
- **Other tables** are walked sequentially in ranges of `--batch` rows.
- **Stored digests.** After a verified run, the target's range digests are stored in `tether_migration_ranges` (class A). The next run takes them into memory and clears the table, so a run that stops part way leaves none behind. A range whose source digest equals its stored digest is not re-read on the target, so the final run reads only the source for the unchanged ranges. The trade-off: between runs only migrate-from may write the target. A change made by anything else in between is found by `--verify`, which walks every range on both sides, and not by the final run.
- **Verification after a run** rechecks only the ranges the run wrote, in parallel (`verify_mode: incremental`). `--verify` walks every range (`full`).
- **Target check.** `--target-check full|quick|none` sets the check run on the result. The window run can use `none` and run `integrity_check` read-only after the new hub starts.
- **Hashing.** The source is hashed concurrently with the copy and the verification.
- **Families.** `--family a|b|all` (default `all`). A complete database is never rewritten, so the final run must cover every family.

**Triggers are off during the copy.** A copied row is history, not a new event. Without this, inserting `messages` would write journal rows, and `principals` would be re-checked against today's sessions.
- SQLite: each trigger on a copied table is saved in `tether_migration_triggers`, dropped, and recreated after the copy. A run that stopped mid-copy has its triggers recreated first.
- Postgres: `ALTER TABLE ... DISABLE TRIGGER USER` per table (owner rights; `session_replication_role` would need superuser).
- Either way, the state is recorded in `tether_migration_triggers` before it changes. Triggers are put back even when the run is canceled, and a run that stopped part way is repaired by the next start. SQLite triggers are recreated in their original order, which is the order they fire in.

## Environment variables and config keys

New code reads no `os.Getenv` for settings, except `TETHER_DATA_DIR`. Each slice adds the old variables it retires or maps.

| Old variable / key | New | Slice |
|---|---|---|
| (none) | `TETHER_DATA_DIR` (required) | S0 |
| catalog `defaults.state_db` | `database.sqlite.path` (default `tether.db` in the data directory) | S0 |
| catalog `daemon.events_retention.{enabled,days,debug_days}` (defaults true, 90, 7; 0 or negative disables) | `tether.*` retention keys with the same defaults and semantics, pinned by a test | S8 (not in S0 on purpose: a wrong default deletes live history) |
| `TETHER_CATALOG_PLAN_TTL` (service descriptor env) | to map | catalog slice |
| `TETHER_LAUNCH_HOST` (unit drop-in) | to map | S9/S10 |
| `HOLLIS_OTEL_DISABLED` (sysop unit) | dropped with sysop (D4) | S0 note |

## CLI start-up (for S14)

At S0, `run()` boots the modules before cobra parses the arguments, so every subcommand (even `--help` and `version`) needs `TETHER_DATA_DIR` and creates the data directory. That matches the guide's pattern and is fine while `serve` is the only command. S14 CLI parity must change it for the commands agents run by name: `tether mcp --forward-daemon` and `tether mailbox watch` must not open or create a data home, as today.

## Dropped

- **LLM gateway:** `internal/llm`, `modelcatalog`, the `/ai/*` routes and the `tether_ai_*` tools. Its data is archived per D3.
- **sysop** (`apps/sysop`, D4).
- **Never-served packages:** `internal/enrollment`, `fabricbinding`, `fabricenrollment` (no production importer). Their tables are still migrated if they hold rows.
- **`internal/otel`: dropped.** Checked at S0 on 2026-11-14:
  - The old code exports OTLP to the default `localhost:4318` unless `HOLLIS_OTEL_DISABLED` / `TETHER_OTEL_DISABLED` is set, or to `OTEL_EXPORTER_OTLP_ENDPOINT`.
  - The running deployment was checked at S0: no exporter is configured and no collector receives the exports, so they go nowhere today. The observation is recorded outside this repository with the cell's deployment notes.
  - If a collector is introduced later, tracing is a Substrate module question (`substrate-gap`), not app code.
