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
  migrations/          one goose migration set (empty until S1)
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

There is one embedded goose migration set (`migrations/`). It is registered only once it holds a `.sql` file: the module refuses an empty source, and S0 adds no placeholder version for S1 to reconcile. S1 adds the squashed schema.

## HTTP

At S0, `httphost` binds `127.0.0.1:8949` (loopback; a port neither old listener uses) and serves `/healthz`:

```json
{"status":"ok","checks":{"database":"ok"}}
```

S4 replaces this with two listeners:
- the unix socket at its old path;
- the remote listener at its old loopback address, with the closed client mount list.
