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

### D6: socket admission stays as today

**Status:** recommended (a).

Today `identity.mode` is `observe`:
- **Socket callers** (planted MCP forwarders, `mailbox watch`, the CLI, `go-tether-client` over UDS) are admitted even with no token or a stale one.
- **The remote TCP listener** always requires a device credential.

**The port keeps that per listener:** the socket at Substrate `permissive` (log what enforce would refuse, refuse nothing), the remote listener enforcing. If the auth module cannot hold a different mode per listener, a `substrate-gap` task is filed, and the remote listener's enforcement becomes a tested check inside its mounts, as it is today. S4 proves it.

## Data directory and the old layout

| Old | New |
|---|---|
| Two roots from the old catalog (`defaults.state_db`, for example `<data root>/state/tether.db`), plus XDG via apppaths name `tether` | `TETHER_DATA_DIR` (required, a new directory beside the old data root); database `tether.db` inside it |
| `<data root>/catalog` | unchanged, same path (clients name it) |
| `<data root>/run/tetherd.sock` | unchanged, same path (S4 configures it explicitly) |
| `<data root>/auth/` (issuer) | carried byte for byte into the new data directory (S3/S12) |

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
| `TETHER_CATALOG_PLAN_TTL` (hub descriptor env) | to map | catalog slice |
| `TETHER_LAUNCH_HOST` (unit drop-in) | to map | S9/S10 |
| `HOLLIS_OTEL_DISABLED` (sysop unit) | dropped with sysop (D4) | S0 note |

## Dropped

- **LLM gateway:** `internal/llm`, `modelcatalog`, the `/ai/*` routes and the `tether_ai_*` tools. Its data is archived per D3.
- **sysop** (`apps/sysop`, D4).
- **Never-served packages:** `internal/enrollment`, `fabricbinding`, `fabricenrollment` (no production importer). Their tables are still migrated if they hold rows.
- **`internal/otel`: dropped.** Checked at S0 on 2026-11-14:
  - The old code exports OTLP to the default `localhost:4318` unless `HOLLIS_OTEL_DISABLED` / `TETHER_OTEL_DISABLED` is set, or to `OTEL_EXPORTER_OTLP_ENDPOINT`.
  - The live hub unit sets no `OTEL_*` variable, the catalog's `global.yaml` has no telemetry key, and nothing listens on 4318 on the hub host. Every export goes nowhere today.
  - If a collector is introduced later, tracing is a Substrate module question (`substrate-gap`), not app code.
