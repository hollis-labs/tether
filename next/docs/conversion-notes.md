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

### D6: socket admission stays as today

**Status:** recommended (a).

Today `identity.mode` is `observe`:
- **Socket callers** (planted MCP forwarders, `mailbox watch`, the CLI, `go-tether-client` over UDS) are admitted even with no token or a stale one.
- **The remote TCP listener** always requires a device credential.

**The port keeps that per listener:** the socket at Substrate `permissive` (log what enforce would refuse, refuse nothing), the remote listener enforcing. If the auth module cannot hold a different mode per listener, a `substrate-gap` task is filed, and the remote listener's enforcement becomes a tested check inside its mounts, as it is today. S4 proves it.

### Postgres baseline: decisions and open items (S1b)

**Status:** the mapping rules are decided (CW-20261114-0175, accepted by the cell lead). Foreign key enforcement is open.

Decided rules for `migrations/postgres/`:
1. **Timestamps stay text**, `TEXT COLLATE "C"`, in the same format as on SQLite. The Go code, digests and the `migrate-from` copy are then the same on both dialects. A move to `timestamptz` is a later migration, column by column.
2. **No foreign keys are declared on Postgres.** See "Foreign key enforcement" below.
3. **JSON keeps its declared type.** JSON in `TEXT` stays `text`, JSON in `BLOB` becomes `bytea`, and triggers and indexes cast with `tether_json()`. Moving the team tables' BLOB columns to `jsonb` goes with S2's query port.
4. **`INTEGER` becomes `BIGINT` everywhere**, flags included. `REAL` becomes `DOUBLE PRECISION`. A rowid-alias key, with or without `AUTOINCREMENT`, becomes `BIGINT GENERATED BY DEFAULT AS IDENTITY`.
5. **Other constructs.** `UNIQUE COLLATE NOCASE` becomes a unique index on `lower()`. `GLOB` becomes `~`. FTS5 becomes a `tsvector` with a GIN index, using the `simple` configuration. Each trigger becomes a plpgsql function.
6. **`messages.seq` is `00002` in both sets**, so `00001` stays as approved.

**Foreign key enforcement (open, decision for @archie).** The old store never turned `foreign_keys` on. go-store's SQLite opener turns it on, and `store.SQLiteConfig` has no option to turn it off. As things stand, the new SQLite store enforces the 48 declared references and their `ON DELETE CASCADE` / `SET NULL`, while the Postgres baseline declares none.

Today's snapshot holds 7 orphan rows:
- `runtime_bindings` -> `sessions`: 4
- `session_refs` -> `sessions`: 3

The decision is between two routes:
- **Off on both.** This is today's behaviour, and it needs a go-store option exposed through the database module (a libs and Substrate gap).
- **On on both.** Add the foreign keys to the Postgres baseline. `migrate-from` cleans the orphans first. S2 reviews every cascade before porting the delete paths, because deleting a session would then remove its launch plan, MCP policy, bindings, refs, provider mappings, idempotency rows and shim, where today they stay.

Turning enforcement on needs three things:
- a clean `PRAGMA foreign_key_check` on the cutover copy;
- the cascade review;
- the same constraints on both dialects.

Known differences between the dialects, handled in the slices that port the queries:
- **Identity gaps.** An identity value is spent by a refused or rolled-back insert; an `AUTOINCREMENT` value is not. Ids keep their order but can have gaps on Postgres, so nothing may read a gap as a deletion.
- **`seq` after deleting the newest message.** On SQLite, `messages.seq` for a new row is `MAX(seq)+1`, which reuses the value of a deleted newest row, exactly as `rowid` did. A Postgres identity never reuses a value. The mailbox watch keeps its old behaviour on SQLite.
- **Timestamp column types.** modernc returns `TIMESTAMP`-declared columns (`settings.updated_at`, `session_idempotency.created_at`) as `time.Time`; Postgres returns the text. The S2 scan code reads them as text on both.
- **Trigger JSON.** The triggers write `message_event_journal.payload_json`, which becomes the emitted event's payload. Postgres builds it as text, byte for byte what SQLite's `json_object` writes. `TestTriggerParity` compares the bytes, including quoting and control characters.
- **Case-insensitive aliases.** Lookups by alias use `lower(alias) = lower(?)`, which serves both dialects. Today's `COLLATE NOCASE` comparisons do not port as written (S2).
- **Forum search.** Search queries differ per dialect: FTS5 `MATCH` against `tsquery` (S11).
- **Statement time.** `tether_now()` and `DEFAULT CURRENT_TIMESTAMP` use the statement start time, as SQLite's `'now'` does.

Framework items:
- **Version tables.** Substrate #44 (named stores, one goose version table per store) is not in the pinned Substrate (`d064f536`). Both sets use the default `goose_db_version` until the pin moves. The class-B and class-C stores need #44's per-store table before they move.
- **Extensions.** The Postgres tests use go-store's `storetest`, which creates the `pg_trgm` and `unaccent` extensions on the test server. The server needs them, as stock Postgres 17 images provide.

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

```sql
CREATE TABLE tether_migration_mark (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  state        TEXT NOT NULL,      -- 'running' while migrate-from copies, then 'complete'
  source_sha   TEXT,               -- old tree commit the source database was written by
  completed_at TEXT                -- RFC 3339 UTC
);
```

The guard reads only `state` (row `id = 1`). Any value other than `complete`, or a missing row, is an interrupted migration and is refused. The columns besides `state` are S12's to finalize.

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
