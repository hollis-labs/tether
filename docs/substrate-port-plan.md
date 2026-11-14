# Substrate port plan: Tether

Status: **plan, not approved.** No port code is merged before @archie and @substrate-manager approve this plan. Nothing in this
document is run against the live hub, its database or its service unit by this cell; the cutover is run by
@archie with Chrispian (section 9).

Task: Torque CW-20261112-0101 (wave 3, in use, cutover). Cell: mig-tether (lead @mig-tether-lead, builders
@mig-tether-builder-1 and -2, reviewer @mig-tether-review). Method: hollis-labs/substrate
`docs/adopting-substrate.md`, section 11 for the cutover. Prior art read: hollis-labs/station
`docs/cutover-plan.md` (two units, ~50 MB of data, one-shot `migrate-from`).

Tether is last in the cutover order and the most used app in the fleet: every agent session, every mailbox,
every MCP tool call from an agent, and the outposts that host sessions go through it. The plan is written for
that: the window is short, every byte of state is accounted for, and rollback loses nothing.

Inventories this plan relies on (built by the cell, regenerated before cutover):

| Inventory | Task | File (the cell's shared directory on the hub host) |
|---|---|---|
| Package/LOC map with dependency layers | CW-20261114-0087 (done) | `package-map.md` |
| Every table and store, with sizes and growth | CW-20261114-0089 | `data-inventory.md` |
| Every client, surface and credential | CW-20261114-0092 (done) | `client-list.md` |

Their summaries are folded into sections 4, 7 and 8. A figure marked *(inv)* comes from an inventory and is
re-measured on the day.

## 0. Decisions asked of @archie (answered first)

The cell decides everything else in this plan. These cross an app boundary, change a contract other apps
depend on, or risk data, so they are asked, each with a recommendation.

| # | Question | Options | Recommendation |
|---|---|---|---|
| D1 | **Issuer continuity.** Tether mints the fleet's agent/session tokens, non-expiring service tokens (`svc-` keys), device-pairing grants and workload trust (`internal/authissuer`), and serves `/.well-known/jwks.json` and a deny-list that other apps poll. Facts: Ed25519/EdDSA; ordinary tokens 15 min (max 60); the signing key rotates every 30 days with 2 h overlap; service tokens carry a 10-year `exp` on a separate `svc-` key and end only by revocation; device credentials are opaque `tth_` tokens stored hashed in `principals`; `auth/state.json` holds the deny-list, session grants, minted ids and service-token records. Torque, Tesseract and Tachyon refresh the JWKS and poll the deny-list every 30 s. Substrate's `identity` module issues its own key format (different `kid` derivation, 15 min to 1 h tokens, no non-expiring service tokens, retired keys expire). | (a) Explicit exception: the agent-identity issuer stays Tether app code (it is Tether's product, not app-local login); the signing key directory is carried verbatim, so every issued token keeps verifying; `identity` is selected only for Tether's own operator accounts and API keys. (b) Replace with `identity`: every agent, outpost, app and service token is reissued in the window. (c) Add Tether's token classes to Substrate `identity` first, then (b) without the reissue. | **(a).** Zero credential changes in the window; the ADR (security_direction_v1) already names Tether the one issuer. (c) is the long-term shape and becomes a Substrate task filed by this cell, not a precondition. @substrate-manager agrees with (a) and with (c) as a later framework task (2026-11-14), with five conditions written into this plan (sections 3, 5, 7, 10): the issuer declared as the one exception under Gaps in `conversion-notes.md` with its open routes named; `auth/` and `state.json` carried byte for byte (checksums before and after, modes kept, old files unwritten), with rotation tested across the cutover boundary; operator accounts and keys through `identity`, with `LocalScopes` excluding any scope an agent must not hold; one verifying path through Substrate `auth`; no change to claims or to the issuer URL. **This is the fleet's trust root, so the decision is Chrispian's through @archie; S3 does not start before it.** No timeline exists for apps to move off Tether-issued tokens; the port assumes they stay. |
| D2 | **HTTP paths.** Substrate puts every route under `/api/` with flat errors; Tether's routes are at the root (`/messages`, `/sessions/...`, `/registry/...`, `/auth/...`) with Tether's typed error envelope (ADR 0010, `docs/api/README.md`). They are called by outposts (`/worker/v1/connect`, `/auth/token`, `/auth/pair/exchange`, remote `/mcp`), by `go-tether-client` and its consumers (Nanite, Hadron, Tangent, Cerberus service tokens on `POST /events`, `/messages`, `/messages/notify`), and by every app that polls `/.well-known/jwks.json` and `/auth/denylist`. | (a) Move to `/api/tether/...` with flat errors; every client changes in the same window (a `go-tether-client` major plus a bump in each consumer, all shipped together, all rolled back together). (b) The client API is Tether's wire protocol and is protocol-fixed (the exception the clean-break decision already makes for A2A): kept at its paths and envelope as declared `httphost` mounts that verify their own callers through the auth module; only routes new to the port (operator identity, setup) live under `/api/`. Moving the client API to `/api/` is a later, versioned change made client by client. | **(b).** Tether is cut over last and is called by every app; (a) turns one app's cutover into a synchronized release of six or more apps with a joint rollback. (b) keeps the window to one app. The mount list is closed and tested (every mount named, every route on it scoped by Tether's own check). |
| D3 | **LLM gateway data.** The gateway is out of scope (Station/Relay replaced it), but its tables (`ai_events` and the budget/usage tables) hold history. | (a) Not migrated into new Tether; archived as a standalone SQLite file next to the moved-aside database, kept until the owner deletes it. (b) Migrated and unused. | **(a).** No row is deleted: the archive is a verified copy (counts, EXCEPT both ways). |
| D4 | **sysop** (`apps/sysop`, the `hub-tether-sysop` unit, a Vite UI on a loopback port). | (a) Not ported; the unit is stopped at cutover and left installed for rollback; a GUI port is a separate later task. (b) Ported in this wave by a GUI agent. | **(a)** unless the client list shows daily use. |
| D5 | **Repository.** Same repo `hollis-labs/tether`, long-lived branch `substrate-port` (created 2026-11-14 from `main` at `f6c37bfa`), or a new repo. | same repo / new repo | **Same repo** (archie's ruling allows either): history, issues, CI secrets and `tether-release` stay; the new tree lives beside the old one on the branch (section 2). |
| D6 | **Socket admission.** Live `identity.mode` is `observe`: callers on the unix socket (the planted `tether mcp --forward-daemon`, `mailbox watch`, the CLI, `go-tether-client` over UDS) are admitted even with no token or a stale one; the remote TCP listener always requires a device credential. Substrate `auth.mode: enforce` on the socket would refuse the first group. | (a) Keep today's posture per listener: the socket at Substrate `permissive` (log what enforce would refuse, refuse nothing: the same contract as `observe`), the remote listener enforcing; enforcing the socket is a later change made once the client list proves every socket caller presents a verifiable credential. (b) Enforce on both at cutover. | **(a).** No admission change in the window. If the auth module cannot hold a different mode per listener, a `substrate-gap` task is filed and the remote listener's enforcement is a tested check inside its mounts, as it is today. |

## 1. What runs today (read from the hub host, names only)

| Unit (systemd --user, written by Cerberus) | Command | Listens |
|---|---|---|
| `hub-tether` (Cerberus resource) | `<install>/tether --catalog <data root>/catalog serve --remote-listen tcp:<loopback port> --allowed-host <two public hostnames> --allowed-origin ...` | unix `<data root>/run/tetherd.sock`, tcp loopback (behind the TLS proxy for the public hostnames) |
| `hub-tether-sysop` (Cerberus resource) | `<install>/tether-sysop -addr <loopback port> -catalog ...` | tcp loopback |
| `catalog-autocommit.path` / `.timer` | commits catalog changes to its git history | n/a |
| per-session helpers | `tether mcp --forward-daemon --daemon-address unix:.../tetherd.sock`, `tether mailbox watch <urn>` (one per live agent) | stdio |

Data root `<data root>` (sizes on 2026-11-14):

| Path | What | Size |
|---|---|---|
| `state/tether.db` (+ `-wal`, `-shm`) | the database (WAL mode) | 11.4 GB + 67 MB WAL |
| `state/catalog-backups/`, `state/backups/` | catalog snapshots and old DB backups | *(inv)* |
| `state/shims/`, `state/tether.db.turn-output-retries/` | shim state, turn-output retry spool | *(inv)* |
| `state/environment-id`, `.recovery` | this hub's environment identity | 37 B each |
| `auth/` | signing keys (`*.key`, `svc-*.key`), `keys.json`, `jwks.json`, `state.json` | 136 KB, **secret** |
| `catalog/` (git) | agents, projects, providers, launches, boot, mcp-servers, credentials, team definitions, `global.yaml` | 6.1 MB, **carries secrets** (ADR 0041) |
| `launch-specs/`, `lenses/`, `skills/`, `receipts*/` | file state | < 1 MB |
| `run/` | socket, pid, `operator.token` | **secret, runtime** |
| workspace and temp roots | workspace and temp roots named by `global.yaml` | *(inv)* |

**More than one process opens `tether.db` today.** The daemon owns it, but plain `tether mcp` (without
`--forward-daemon`/`--daemon-only`) opens it through `app.New`, `tether_sysop` opens it read-write
(`apps/sysop/cmd/tether_sysop/main.go`), and the CLI commands `init`, `identity rotate`, `identity unbounded` and
`workspaces` open it with `store.Open`. The live per-agent processes are `--forward-daemon` and `mailbox watch`
and open nothing. The new build has exactly one opener, the hub; every other command goes through the API. In
the window every opener is stopped, not only the hub unit.

`identity.mode` in the live catalog is `observe` (not enforce); `permission_mode: bypass` is the fleet dev posture.
Sessions launched on the hub host itself run `direct` (no `launch_host` set): they are children of the daemon
and stop with it; the next start marks them failed and resumes only those with `recovery: auto`. Sessions on
outposts are held by the outpost's shim host and survive a hub restart (resume cursors on reconnect).
An older layout (`~/.tether/state/tether.db`, 387 MB) exists; the inventory says whether anything still reads it.

## 2. Repository, branch and how old `main` is carried across

- **Branch.** Every port PR targets `substrate-port` in `hollis-labs/tether`. Merges go through
  @substrate-manager, the Substrate program driver (routing set by Chrispian, 2026-11-14): when the cell's
  reviewer approves at the head SHA and CI is green, the lead sends @substrate-manager the PR URL and the
  reviewed SHA and follows its merge process (squash, matched to the reviewed head). Deploys go the same way;
  anything touching the live app and the cutover stays with @archie.
  This plan (docs only) goes to `main`. The port reaches `main` only at cutover, run by @archie with Chrispian.
- **The new tree lives beside the old one on the branch**, as Station did with `cmd/station-next`: a nested Go
  module `next/` (`module github.com/hollis-labs/tether/next`, its own `go.mod` on the Substrate BOM pins, its own
  CI job). The old tree is not edited on the branch, so:
  - `git merge origin/main` into `substrate-port` never conflicts on old code;
  - the new module cannot import the old `internal/` packages (Go forbids it across modules): the clean break is
    enforced by the compiler, not by review;
  - the old root CI keeps passing on the branch, and `next/` has its own gate (Substrate section 12 evidence).
- **Merge cadence.** The lead opens a PR merging `origin/main` into `substrate-port` at least daily while photon (or anyone)
  lands PRs on `main`, and always before a slice that touches the same area starts. That merge is its own PR, merged like any other,
  (`chore: merge main @<sha>`), reviewed for conflicts only.
- **The carry ledger.** Each `main` commit that arrives by a merge is recorded in `next/docs/carry-ledger.md`:
  sha, PR, task, area, and one of `ported` (the slice that carries it, with the commit), `n/a` (touches only code
  the port drops: LLM gateway, sysop, old auth plumbing, per-version migrations), or `open`. A slice is not done
  while a ledger row in its area is `open`. The cutover gate (section 9, step zero 6) is: zero `open` rows up
  to the `main` SHA being replaced. Behaviour changes are carried as behaviour (code plus the test that proves
  it), never by copying files across trees.
- **Photon coordination.** The photon cell lands messaging PRs on `main` now. Through @archie: photon tags its
  PRs' Torque tasks `carry:mig-tether`; the messaging slices (S6, S7) start after the last photon messaging PR
  that is in flight on the day the slice starts, and the ledger catches anything later. Schema changes on `main`
  (new migrations `0102+`; `0099`-`0101` already exist at `f6c37bfa` and are in S1's squash) are the high-risk carries: each one gets a ledger row naming the transform in the
  migration tool (S12) as well as the new schema. Known in flight on 2026-11-14: address aliases #554 and #555
  (CW-20261114-0067/-0068, a new canonical-address model: a schema and a wake-path change), and the open PRs
  #556-#559 (session reaping, dead hosts, hook permissions, input triage). Each gets a ledger row when it lands. The branch `fix/gateway-010-acceptance-findings` named in the task landed as #473 (squash-merged; its head `d74467fb` is
  not an ancestor of `main` only because of the squash), so it is already in the tree the port starts from.
- **Promotion at cutover.** The final slice (S15) on the branch deletes the old tree, moves `next/` to the root,
  rewrites the module path to `github.com/hollis-labs/tether`, and points `tether-release` and CI at it. That
  commit is what @archie merges to `main` in the window.

## 3. Target layout on Substrate

```
next/
  cmd/tether/              main.go, substrate.yaml, substrate_wiring.go (generated, committed)
  cmd/tether-worker/       unchanged protocol client (only if the outpost runner still uses it; see section 5)
  internal/settings        the tether: config block, validated at start
  internal/<domain>/...    ported domain packages (section 4)
  internal/migrate         migrate-from: old database -> new database (S12)
  migrations/              one squashed goose schema (no per-version ladders)
  docs/                    architecture.md, conversion-notes.md, cutover-plan.md, carry-ledger.md
```

- **Binary:** one `tether` binary, variant `service` (no UI; `httphost` for the API, `mcp` for tools, `cli` for
  the operator commands). `tether serve` is the hub. The local stdio MCP forwarder and `mailbox watch` remain
  subcommands of the same binary because agents on outposts invoke them by name *(inv: client list)*.
- **Manifest** (`next/cmd/tether/substrate.yaml`): `profiles: [agentic]`, `modules.add: [httphost, database,
  auth, identity, secrets, policy, mcp, scheduler, queue, events, cli]`. Config defaults in a `tether:` block
  (catalog root, state roots, retention days, ingest flags, mcp upstream table location, worker hub limits).
- **Database:** SQLite through the `database` module (`database.From(app)`, one writer, read pool); one embedded
  goose migration set. Tether stays SQLite-only (its SQL uses SQLite syntax); recorded in `architecture.md`.
- **Data directory and fixed paths.** The new data home is a **new directory beside the old data root**
  (`TETHER_DATA_DIR`, set explicitly by the descriptor; resolved in `AfterConfig`; absolute paths written into
  `app.Config`; directories 0700, database and keys 0600). It holds the new database, the carried `auth/`
  directory, `operator.token` and the file state. **The catalog stays where it is** (`<data root>/catalog`, git,
  autocommit units unchanged): clients pass `--catalog <data root>/catalog` by path (planted MCP configs, Nanite's
  stored server row, the `chief` agent, `tether-remote`), it is configuration shared by both builds, and its
  format is frozen (section 5.3), so the old hub reads it unchanged on rollback. The **socket and the remote listener address are
  client contract and stay where they are**: the socket is configured explicitly at the old path
  (`<data root>/run/tetherd.sock`) and the remote listener at the same loopback address, so planted MCP configs,
  `mailbox watch` and the proxy need no change. The old root's `run/` directory is the only old path the new
  hub writes (a runtime socket and pid, recreated at every start, never data). A startup guard
  (`Before["database"]`) refuses a data home that holds the old layout (a `tether.db` without a `complete`
  migration mark, section 7.2), so the new build never opens the old file in place.
- **Auth.** Remote listener enforcing; socket per D6. **Two trust roots in one process:** the `identity`
  module (operator accounts and keys, its own key) and Tether's issuer (D1). Both are verified by the one
  Substrate `auth` path (substrate-manager's condition 4). The issuer's key ring rotates (30 days) and its
  deny-list lives in-process, so pointing `auth.jwks_url`/`denylist_url` at the hub itself is circular at start
  and `bootstrap_key_file` holds one static key. The port needs an in-process key and deny-list source for the
  auth module; this is filed as CW-20261114-0097 (`substrate-gap`) rather than worked around in S3; option (c) of D1 is CW-20261114-0098. Operator accounts and operator API keys from
  `identity` (offline bootstrap). Scopes keep the existing `tether.<verb>` vocabulary (`tether.read`, `.operate`, `.messages`, `.worker`, ...); the full list is mapped in
  `conversion-notes.md`, and every route has one (test that fails on an unscoped route).
- **Stdio MCP forwarder:** `mcp.ServeStdio` with `LocalTrust` is **not** used for the forwarder: it serves one
  agent per process but acts for that agent's identity, so it carries the agent's token as today.

## 4. Module mapping

Old package groups (174k non-test LOC, 188k test LOC in Go; the package map has every package) and where each goes.

| Old packages | LOC (non-test) | Goes to | Notes |
|---|---|---|---|
| `internal/config`, `settings`, `setup`, `secretsetup` | 4.6k | Substrate `config` + `internal/settings` + `secrets` | env vars and catalog keys mapped in conversion-notes |
| `internal/store` (custom numbered migrator, `schema_migrations`) | 15.9k | Substrate `database` + ported query code; one squashed schema | per-version ALTER ladders dropped |
| `internal/api`, `cmd/tether` HTTP wiring, `internal/service`, `daemon` | 32k | Substrate `httphost` + ported handlers | client API as protocol-fixed mounts (D2); new operator routes under `/api/` |
| `internal/app` (composition root) | 21.9k | split: wiring to `boot`/manifest; domain logic to its domain packages | the largest single piece; sliced by domain, not moved whole |
| `internal/authissuer`, `identity`, `devicepair`, `enrollment`, `sshenroll`, `credfile`, `apikeyhelper`, `cmd/tether-apikey-helper` | 7.1k | per D1: issuer stays app code; operator login/keys to `identity`; verification to `auth` | |
| `internal/messaging`, `broker`, `outpostmail`, `forums`, `federation`, `pulse` | 4.9k | ported | photon's active area: carry ledger |
| `internal/registry`, `fabricstore`, `fabricbinding`, `fabricenrollment`, `userprofile` | 10.7k | ported | registry keeps ADR 0041 (no payload caching) |
| `internal/session`, `sessionhealth`, `sessionpeek`, `checkpoint`, `launch*`, `specresolve`, `definitionresolve`, `bootgen`, `bootexec`, `launchartifacts`, `workspace` | ~7k | ported | |
| `internal/shim*` (host, codex, agy, bridge, retire), `acpadapter`, `acpsvc`, `a2aadapter`, `pkg/claudestream` | 9.6k | ported | runs agent CLIs; behaviour-sensitive |
| `internal/team*` (host, runtime, svc, store, policy, import, cli, loop, activation) | 7.6k | ported | |
| `internal/workerhub`, `workerruntime`, `workerwire`, `environment`, `environmentstream`, `cmd/tether-worker` | 7.3k | ported **byte-compatible** (section 5) | the outpost protocol is frozen |
| `internal/mcpgateway`, `mcpadapter`, `mcpforward`, `mcptransport` | 21.7k | Substrate `mcp` for Tether's own tools; the upstream gateway (proxying Torque, Tesseract, Cerberus ...) stays app code | `mcp.UseToolFilter` for per-caller tool lists |
| `internal/events`, `eventingest`, `eventretention`, `eventstamp`, `agentevents`, `telemetry`, `otel` | 5.9k | `events` module for SSE fan-out; ingest and retention ported; retention as a `scheduler` job | |
| `internal/catalog*`, `launchprofile`, `launchresolve`, `skills`, `execpolicy`, `teamspolicy` | ~5k | ported; policy decisions through `policy` (shadow first) | catalog stays git-backed files |
| `internal/client`, `teamcli`, the `cmd/tether` CLI commands | 5.7k + CLI | ported on `cli` (cobra) | the Go client moves to the new paths |
| `internal/llm`, `provider`, `modelcatalog`, `/ai/*` routes, `tether_ai_*` tools | 6.2k | **dropped** (out of scope) | data per D3 |
| `apps/sysop` | 5.9k + UI | **not ported** (D4) | |
| `internal/agent`, `agentops`, `callcontext`, `environment`, `testutil`, small helpers | ~1.5k | ported where used | |
| `internal/enrollment`, `fabricbinding`, `fabricenrollment` | 1.9k | **dropped**: no production importer (inert primitives; never-served code is not a port target) | their tables, if any hold rows, are still migrated |
| `internal/otel` | 0.1k | dropped unless the live catalog enables an exporter (checked in S0) | otherwise a Substrate gap task |
| `internal/daemon` (UDS listener, `tetherd.sock`) | 1.9k | ported on `httphost` over `unix:` (accepted by `ValidateBind`); S4 proves the socket and its peer-credential check | a Substrate gap task if httphost cannot |

The package map (CW-20261114-0087, main `f6c37bfa`) counts 121 packages and 178,373 non-test LOC with no import
cycles; the table groups them.

## 5. What stays app code, and what is frozen

1. **App code (Tether's domain):** messaging, registry, sessions and shims, teams, the worker hub, the MCP
   upstream gateway, the catalog, event ingest and retention, and the agent-identity issuer (D1).
2. **Framework (deleted from Tether, not ported):** config loading, logging setup, health, HTTP host and host
   checks, database open/migrate, scope guard and JWT verification, operator login and API keys, secrets
   resolution, the stdio/HTTP MCP server plumbing, SSE plumbing, the scheduler loop.
3. **Frozen protocol (byte-compatible, proven by tests carried from the old tree):**
   - the outpost/worker wire: WebSocket `GET /worker/v1/connect`, JSON frames typed by `t`, `Tether-Protocol: 1`,
     `hello` with `proto == 1` (a mismatch is closed silently, so a change would strand every outpost without an
     error), ping 15 s / dead at 45 s, resume cursors and kill lists; the device `tth_` credential with scope
     `worker`; `POST /auth/pair/exchange` and `POST /auth/token` as outposts call them; remote `/mcp` with a bound
     agent JWT;
   - the outpost mail delivery contract (`deliver{cmd_id,message_id,urn,binding,text}`);
   - tokens: Ed25519/EdDSA, claims `iss, sub, aud, scope, exp, iat, jti, kind, sid, par`, every token class, the
     `svc-` key class, JWKS at `/.well-known/jwks.json`, the deny-list document at `GET /auth/denylist`;
   - the client API paths and error envelope (D2), and MCP tool names and argument shapes (agents and planted MCP
     configs call them by name), including `search` discovery (`tether_tool_search/list/call`) and `/p/<profile>`.
   - the **error envelope** `{"error":{"code","message"}}` (ADR 0010) on every client mount: the mounts are
     declared `Mount{Raw: true}` so `apierr.Normalize` does not flatten them. Refusals that still answer flat
     under `Raw` (the host check, and the guard and `route_not_mapped` where they apply) are listed per client
     in section 8, or shown unused by any client;
   - the **on-disk `auth/` format** (`keys.json`, `<kid>.key`, `svc-<kid>.key`, `jwks.json`, `state.json`): the new
     issuer reads and writes the same layout, so the directory round-trips in both directions (rollback);
   - the **socket path and the remote listener address** (section 3), and the CLI invocations agents run by name:
     `tether [--catalog <dir>] [--token-file <f>] mcp --forward-daemon --daemon-address unix:<sock>
     --discovery-mode <m>` and `tether [--catalog <dir>] mailbox watch <urn> --json`, flags accepted as today.
   - **addresses and paths clients hold literally** (client list, CW-20261114-0092): the binary path
     (`<install>/tether`: planted MCP configs, Nanite's stored server row, the `chief` agent, `tether-remote`,
     outposts' relay), the catalog path and the catalog file format (section 3), the public hostname of the
     remote listener (proxy, JWT `iss` consumers, Nanite's issuer URL, outposts' hub URL), and the exact
     `TETHER_ADDR` strings (Tachyon derives its chat identity from them, so a byte change re-keys its chat
     history). The new build installs at the same binary path, with the old file kept for rollback.
   Golden fixtures generated from the old tree (CW-20261114-0094) are replayed against the new handlers in CI,
   including one error response per mount family and an `auth/` directory written by the old issuer.

## 6. Slicing plan

The app is too large for one PR. Each slice is one PR into `substrate-port` (a few thousand lines at most,
tests included), in dependency order; a slice lists its carry-ledger areas. Sizes are non-test LOC of the old
code it replaces. Builders: B1 = @mig-tether-builder-1, B2 = @mig-tether-builder-2.

| Slice | Content | Depends on | Size | Seat |
|---|---|---|---|---|
| S0 | `next/` skeleton: go.mod on BOM pins, manifest, wiring, `internal/settings`, data-dir resolution and old-layout guard, CI job for `next/`, `docs/architecture.md`, `conversion-notes.md`, `carry-ledger.md` | plan approved | small | B1 |
| S1 | Squashed schema: one goose migration reproducing the current schema (minus D3 tables), generated from a fresh old-schema dump and reviewed table by table against the inventory. The `messaging_*` tables that the go-messaging library creates stay the library's (its DDL is not duplicated; the library version is pinned and its schema compared) | S0 | ~115 tables | B2 |
| S2 | Store layer: ported query code on `database.From(app)` (read pool / WriteTx discipline), tests ported | S1 | 15.9k | B2 (+B1 split by table family) |
| S3 | Issuer (D1): key store reading and writing the carried `auth/` layout unchanged, minting, JWKS, deny-list, service tokens, pairing (`principals`, `principals_device_upgrade`, `pairing_grants`), workload trust; both trust roots verified through `auth` (needs the in-process key source gap); identity for operator accounts with `LocalScopes` set; golden tests: old-minted tokens verify on new, a revoked one is refused, new-minted verify on libs auth, and the round trip old `auth/` -> new hub -> rotate/mint/revoke -> old issuer reads it | S0, S1, S2, D1 ruling, the auth gap | 7.1k | B1 |
| S4 | HTTP host: the closed mount list for the client API (paths and ADR 0010 envelope unchanged, callers verified through the auth module, Tether's scope check per route, `Mount{Raw: true}`), operator routes under `/api/` with flat errors, unix socket (at the configured old path) and remote TCP listeners with the exact Host/Origin checks and the D6 posture per listener; SSE handlers (`mailbox watch`, `/events/stream`) watch `httphost.Stopping(r)`; the coverage test (every route scoped, every mount named); SIGTERM drains with streams open | S0, S3 | part of 32k | B1 |
| S5 | Registry and fabric (registry, bindings, scoped bindings, fabric*) | S2, S4 | 10.7k | B2 |
| S6 | Messaging core: messages, inbox/consume/ack, threads, aliases, wake admission, retention candidates, purge audit | S2, S4, S5 | ~5k | B1 |
| S7 | Groups, channels, forums, mentions, federation, outpost mail | S6 | ~4k | B2 |
| S8 | Events: store, SSE via `events`, ingest, retention as a scheduler job, tool metrics, proxy events | S2, S4 | 5.9k | B2 |
| S9 | Sessions and launch: session lifecycle, launch resolve/specs/artifacts, workspaces, checkpoints, health, peek | S5, S8 | ~7k | B1 |
| S10 | Shims and runtimes: shim host, Codex, AGY, bridge, retire, ACP, A2A, claudestream | S9 | 9.6k | B1 |
| S11 | Worker hub and environment: `/worker/v1/connect`, workerwire, workerruntime, environment streams; golden replay of recorded outpost sessions; the hijacked WebSocket watches `httphost.Stopping(r)` and sends a close frame, SIGTERM drains with outposts connected | S9 | 7.3k | B2 |
| S12 | `migrate-from` and `export-legacy` (section 7, section 10): range-digest sync in both directions, verification report, D3 archive. Lands incrementally: the engine first (after S1), then one sub-PR per table family as its domain slice lands; complete only after S14 | S1, then each domain slice | new | B2 |
| S13 | Teams (host, runtime, svc, store, policy, import, loop, activation) | S9, S10 | 7.6k | B1 |
| S14 | MCP: Tether's own tools on the `mcp` module (names frozen), the upstream gateway and forwarder, per-caller tool filter; catalog apply/plans; CLI parity | S6-S13 | 21.7k + CLI | B1 + B2 (tools split by family) |
| S15 | Promotion: delete old tree on the branch, move `next/` to root, module path, CI, `tether-release`, Cerberus descriptor draft; final docs | all, ledger clean | mechanical | lead |

Large slices (S2, S10, S14) are split further into sub-PRs by package when a builder starts them; each sub-PR
gets its own Torque task. Two tasks in flight per builder at most. The prep tasks (package map, inventory, client
list) feed S1, S12 and section 8.

## 7. Data migration

**Rule: every byte of state is either migrated, archived (D3), regenerated from a migrated source with proof, or
handed to the operator as a secret. Nothing is dropped.** The inventory (CW-20261114-0089) lists every table and
store with its class; this section is the method.

### 7.1 Classes

| Class | What | Method |
|---|---|---|
| copy | rows whose shape is unchanged | `INSERT INTO new.t SELECT ... FROM old.t` column by column (named, never `*`) |
| transform | rows whose shape changes (renamed columns, merged tables, the `*_new`/`*_v2` leftovers, JSON reshapes) | Go transform per table, unit-tested on fixtures from the copy |
| archive (D3) | LLM gateway tables | copied verbatim into `llm-archive.db`, verified, not loaded into new Tether |
| file state | `auth/` (`keys.json`, `*.key`, `jwks.json`, `state.json`: byte for byte, checksums before and after, modes kept), `run/operator.token` (held literally by Torque, Tachyon and ops scripts: carried byte for byte, never reissued in the window), `launch-specs/`, `lenses/`, `skills/`, `receipts*/`, `state/environment-id*`, `state/shims/`, turn-output retry spool, workspaces | copied by the operator's script with `cp -a` , sha256 manifest before and after; secrets never printed |
| in place | `catalog/` (git) | not copied: read and written in place by whichever hub runs (format frozen); a git tag `pre-substrate-cutover` and a tarball are taken in step zero |
| runtime | `run/` socket and pid | not copied; recreated at start at the same paths |

### 7.2 The tool

`tether migrate-from --old-db <copy> [--phase bulk|final]` in the new binary (S12):

- Opens **a private copy** of the source read-only (`mode=ro`, `immutable=1`); never the live path. Records
  `source_sha256` before and after; `source_unchanged: true` is required.
- Copies in dependency order, in batches (`WriteTx` per batch, cursor closed before writing), with a progress
  line per table.
- **Migration mark with two states:** `bulk@<per-table marks>` after a bulk run, `complete@<source sha256>`
  after a final run that verified. `serve` and the old-layout guard require `complete`; a bulk-only database
  never serves. A crash or a failed verification mid-run means the target file is discarded and the run
  starts over (batches leave a partial database by design; nothing resumes from one).
- Verifies per table: row counts; `EXCEPT` both ways on the copied columns (after the transform, against the
  transform applied to the source); max ids and sequences; per-key-range digests (sha256 of ordered canonical
  rows per range of the primary key) and a whole-table digest; then `PRAGMA integrity_check` and
  `foreign_key_check` on the result. Exit non-zero on any mismatch.
- Prints a report (JSON + text) kept with the cutover record.

### 7.3 Size and the window: bulk, then a range-digest sync

11.4 GB is far larger than Station's 50 MB. The online backup itself cannot be taken with the `sqlite3` shell's
`.backup` while the hub runs: it copies 100 pages a step and restarts on every write, and on 2026-11-14 it did
not finish in 11 minutes (CW-20261114-0089). Pre-window copies are therefore taken from the hub's own daily backup
(the `hub-backup` timer already copies the live file with `VACUUM INTO` in about 60 s and checks it); the cell never
opens the live file. The window copy is taken with the hub stopped. The
inventory and rehearsal give the times. If the rehearsed full migration plus its verification exceeds 10
minutes:

1. **Bulk phase, before the window (old hub running):** that day's `hub-backup` copy is migrated in full
   (`--phase bulk`).
2. **Final phase, in the window (old hub stopped):** a fresh copy of the stopped database. No table is assumed
   append-only: in the old tree `events` is updated in place (outbox `pending`, `event_id` backfill, retention
   compaction of `payload_json`), and retention deletes from `events`, `proxy_events`, `identity_audit`,
   `turn_inputs`, `event_ingest_digests` and `a2a_tasks`; `messages` rows are deleted by routing replies and
   purges. So `--phase final` syncs **every** table by key range: for each range of the primary key it compares
   the source digest with the target digest; equal ranges are kept, different ranges are deleted and re-copied
   from the source, keys above the last range are inserted, and keys present in the target but absent from the
   source are deleted. Small tables are simply re-copied in full. Inserts, in-place updates and deletes below
   any mark are all caught by construction. A table with no usable key order (no primary key, or implicit rowids
   that are reused after deletes) is re-copied in full and verified with `EXCEPT` both ways whatever its size;
   the inventory names each such table with its size, and if a large one exists S12 proposes a key for it.
3. **Verification in the window** is the per-range digests (already computed for the sync, so no second pass
   over unchanged ranges), counts, a whole-table digest, `EXCEPT` both ways on every table under the size
   threshold and on every re-copied range of the large ones, and `integrity_check`. Its time is part of the
   window budget and is measured in rehearsal.
4. Optional, if rehearsal shows the final sync is still too slow: @archie pauses event retention on the live
   hub between bulk and window (a catalog setting, his call), which shrinks the changed ranges. The plan does not
   depend on it.

### 7.4 Rehearsal (on copies, never the live paths)

In the cell's scratch directory on the hub host, with `HOME`, `XDG_*` and `TETHER_DATA_DIR` pointed into the
scratch directory:

1. Copy the latest `hub-backup` output into scratch (7.3); `integrity_check` ok; sha256 recorded. The live file is never opened
   any other way.
2. `migrate-from` full, three runs: counts, EXCEPT 0/0, digests equal, integrity ok, timings of copy and of
   verification recorded.
3. Two-phase: bulk on snapshot A, final on a later snapshot B; the result's digests equal a full run on B. To
   prove deletes and updates are caught, a retention sweep and message purges are run on a scratch copy of A
   with the **old** binary before taking B.
4. The new binary serves the migrated database in the scratch home, and the **real clients** are driven against
   it: the planted `tether mcp --forward-daemon` invocation and `mailbox watch` exactly as agents run them
   (with and without a token, per D6; a tokenless socket caller must resolve to the **same principal** as under today's `observe`, since data ownership follows the principal), `go-tether-client` calls, a `tether-worker` outpost connecting over
   `/worker/v1/connect`, and every MCP tool family with migrated data (inbox, thread, registry lookup, session
   list, workstream, forum, events history), one write per family.
5. Issuer: a token minted by the old binary on the scratch `auth/` verifies on the new hub; a revoked one is
   refused; a forced rotation on the new side, then the old binary reads the directory back (D1 conditions).
6. Rollback rehearsal (section 10) on the same scratch data, after writes of every kind on the new side
   (mail read/consumed/archived, a purge, session transitions, a registry deregister, a binding revoke, a
   device paired, a service token issued and one revoked).
7. File-state copy script run against copies of `auth/` and `catalog/` in scratch: manifests equal, modes kept.

Rehearsal results are recorded in `next/docs/cutover-plan.md` (the record section), as Station did.

## 8. Clients and what changes for each

Full list with evidence: CW-20261114-0092 (44 clients plus 20 gateway upstreams, swept 2026-11-14 against the
live hub and `main` `f6c37bfa`, with the commands to rerun before the window). The decisive point is the one in
section 5.3: keep the hostname, the socket path, the binary path, the catalog path and the credentials, and
almost every row needs nothing.

| Family | Rows (live) | Change at cutover |
|---|---|---|
| Edge and supervision: Cerberus `hub-tether` / `hub-tether-sysop`, the TLS proxy | 5 (4) | descriptor: new command and env for `hub-tether`; `hub-tether-sysop` removed (D4); the Sysop hostnames retired or repointed in the proxy; the remote listener's hostname and port unchanged |
| Apps: Nanite (delivery bridge, send, registry, JWT verify, events), Torque (executor, capacity, forums proxy, judge, JWT verify, outbox), Tachyon (server, control, card events, console), Tesseract (JWT verify, deny-list), Cerberus (events), Tangent (send-only), Hadron, Basalisk, Station, Caster | 22 (17) | none for every live row once D1 and D2 hold, **except Torque's judge** (`POST /ai/decide`, the LLM gateway, not ported) |
| Agents and MCP clients: planted session MCP configs, skills (tool names), tether-mail on the hub host and the Mac, pulse hooks | 10 (8) | none (tool names, CLI invocations and paths frozen). The Mac's `tether-remote` is checked on the Mac before the window |
| Outposts / `tether-worker` | 2 (1) | none (protocol, device tokens and hub URL frozen) |
| Ops scripts (dogfood driver, video-suite gateway check, hub deploy script, Parallax snapshot capture) | 5 | the hub deploy script is replaced by `fleet-install`; Parallax's snapshot capture reads the Sysop API, which goes with D4 |
| Gateway upstreams Tether calls (Torque, Tesseract, Hadron, Nanite, Cerberus, Tangent, Tachyon, ... ) | 20 definitions (18 enabled) | none: catalog in place, same stdio binaries and URLs |
| MCP tool surface | 179 native tools | 165 kept byte-identical; the 14 `tether_ai_*` tools are dropped (D3) |

**Changes other apps must make (to @archie, one list):**

1. **Torque:** the judge calls `POST /ai/decide`. Move it to Station/Relay or disable it **before** cutover.
2. **Cerberus descriptor (hub-fabric):** the new `hub-tether` command and env; remove `hub-tether-sysop`;
   remove the dead `HOLLIS_EVENTS_URL` from `hub-hadron`; drop the inactive legacy `tether-daemon-service`.
   At cutover.
3. **Proxy:** retire or repoint the Sysop hostnames (D4). At cutover.
4. **Parallax:** its snapshot script targets the Sysop API (D4). Before or at cutover.
5. **infra:** `hub-deploy-tether.sh` is replaced by `fleet-install deploy`. At cutover.

Only if a fixed item has to change after all: Nanite's stored MCP server row (binary and catalog path) and
Tachyon's chat identity (`TETHER_ADDR` bytes).

**Found on the way, not caused by the port (to @archie as FYI):**
- The global Claude and Codex `tether` MCP entries are already broken (ssh to a host that no longer
  answers), and they carry inline tokens.
- The agy entry uses a stale October build and the legacy `~/.tether` catalog.
- Torque holds the operator token as a literal value in its env file.
- A periodic tool still opens the legacy 387 MB `~/.tether` database through the legacy default catalog
  (being confirmed in the inventory), so it reads stale data.

Error bodies: client mounts keep the ADR 0010 envelope (section 5.3). The refusals Substrate still answers
flat on a raw mount (an unknown `Host`) are listed per client in the client list; a client that parses the
envelope on that refusal gets a named change here.

## 9. Cutover sequence

The shape follows Station's plan; the driver is @archie with Chrispian. The cell prepares every artifact and
the dry runs; it runs nothing on the live host.

**Step zero (nothing changed):** (1) the new data home does not exist; (2) a one-step online copy of `tether.db` (7.3),
integrity ok, sha256; tar of `auth/`, `catalog/` (and its git), file-state dirs into
the host's pre-change backup root (`<backups>/pre-change/<date>-tether-cutover/`, 0600); (3) the old binary and unit drop-ins copied aside;
(4) the build to deploy named: `substrate-port` head after S15, CI run id, artifacts; (5) the bulk phase of 7.3
done against a recent copy; (6) carry ledger has zero `open` rows up to the `main` SHA being replaced, and that
SHA is `main`'s head on the day (no newer PR landed); (7) every client in section 8 with a change is ready.

**Before the window:** override file (`SUBSTRATE_CONFIG`) written; operator identity bootstrap rehearsed;
`fleet-install deploy tether --dry-run` passes; the Cerberus descriptor change and the proxy (unchanged
hostnames, `X-Forwarded-Proto` once) read and prepared; agents told the window (a broadcast notice; sessions
keep running, mail and tool calls pause).

**The window:**
1. Stop `hub-tether-sysop` (D4), then `hub-tether`, through Cerberus (`service-stop`). Confirm no process holds
   `tether.db` open (`fuser`/`lsof` on the file and its `-wal`): the hub, sysop, any plain `tether mcp` or CLI
   command (section 1). Agents' tool calls and mail now fail fast (the pause starts). Hub-host `direct`
   sessions stop with the hub (as on every hub restart today); the list of them is taken just before, and each
   is resumed after the checks or was set `recovery: auto` beforehand. Outpost sessions keep running.
2. Copy of the stopped database (no writer, so the plain `.backup` finishes); integrity ok; sha256.
3. `migrate-from --phase final` (or a full run, per the rehearsal); report shows every table verified.
4. File state copied with the manifest check; `auth/` and `operator.token` copied byte for byte (D1,
   checksums before and after). The catalog is not moved.
5. Operator identity with the **staged** binary (`$NEWBIN/tether`, extracted from the named CI artifact into a
   scratch directory, not installed): `identity setup --username <owner>` with the password on stdin from the
   owner's passphrase manager (scripted, never an argument), then `identity key create ... --out <file>` (0600).
6. `$NEWBIN/tether validate` against the new data home (opens everything read-only; checks keys, catalog,
   upstreams, the migration mark is `complete`).
7. Install last: `fleet-install deploy tether --commit <sha>` (previous release kept); `/healthz`.
8. Checks (9.1). Then outposts reconnect on their own (protocol unchanged); watch every outpost reattach.

### 9.1 Live verification (in the window)

`/healthz` 200; `GET /api/setup` `setup_required: false`; a token minted by the old hub before the window
verifies on the new hub and a token revoked before the window is refused; JWKS at the old path lists the same
`kid`s and the deny-list document is unchanged; the 30 s pollers (Torque, Tesseract, Tachyon) log no
verification failure across the window; a planted forwarder and a `mailbox watch` on the hub reconnect with no
change to their command line; an unknown `Host` is refused; no credential
is 401; one message sent and read through the public hostname; an agent's inbox shows pre-cutover mail; a tool
call through the MCP gateway to Torque succeeds; every outpost reconnects and its sessions show live; event
counts not lower than at migration; SIGTERM drains within the stop timeout.

### 9.2 Go/no-go and the rollback point

A decision at T+30 min: go (keep) or rollback. After go, rollback still exists (section 10) through the
`export-legacy` full sync.

## 10. Rollback

Stated before the window. The old binary, unit, drop-ins, database and file state are never written by the
cutover; rollback writes a **new** old-format database and sets the pre-cutover file aside, never over it.

**What state exists and how each goes back:**

| State | Where | Back by |
|---|---|---|
| Every table (messages and their read/consumed/archived state, purges, sessions and transitions, registry, bindings and leases, worker cursors and kill lists, workstreams, events, retention deletes) | new database | `export-legacy` (below) |
| Device credentials and pairing | DB rows: `principals`, `principals_device_upgrade`, `pairing_grants`, `identity_audit` | `export-legacy` |
| Signing keys, JWKS, deny-list, session grants, minted ids, service-token records | files: `auth/keys.json`, `*.key`, `jwks.json`, `state.json` | the new `auth/` copied into the old root after the old one is set aside (same on-disk format, section 5.3), checksums recorded |
| Catalog | in place (format frozen) | nothing to move; the old hub reads it as the new hub left it |
| Operator accounts and keys (`identity`) | new database only | not carried back: the old hub uses `operator.token`, untouched |

**Steps:**

1. Stop the new `hub-tether`; remove the drop-in the deploy wrote (or relink the previous release); daemon-reload.
2. `tether export-legacy --old-db <pre-cutover copy> --out <new old-format file>` (S12): the same range-digest
   sync as 7.3, in reverse, with the inverse of every `transform`, over **every** table: inserts, updates and
   deletes made after cutover all go back. Verified like `migrate-from` (counts, EXCEPT both ways, digests,
   `integrity_check`). The pre-cutover file is set aside; the export takes its place.
3. `auth/` and `operator.token` back, as in the table.
4. Start the old unit; check old `/health`; every outpost reconnects; a token minted by the new hub verifies.
5. Credentials: none changed (D1) and no path changed (D2), so no client is touched.

If `export-legacy` fails its verification, the old hub does not start on a half-written file: the failure is
fixed forward on the new hub, or the old hub starts on the pre-cutover file only with the owner's explicit
decision and a written list of what that loses.

**Rollback horizon.** `export-legacy` is supported from cutover until @archie closes the horizon (proposed: the
end of a 7-day quiet period). Until then every schema change on the new side ships with its inverse in
`export-legacy` and a CI test that round-trips it; a change that cannot keep it working closes the horizon
explicitly first. After the horizon, rollback is fix-forward only.

## 11. Descriptors, config and supervision

The Cerberus descriptor (`tether.cerberus.yaml` and the hub-fabric entries) gets: command `tether serve` (flags
gone; settings from `SUBSTRATE_CONFIG`), env `SUBSTRATE_CONFIG`, `TETHER_DATA_DIR`; stop timeout sized to the
drain; `hub-tether-sysop` disabled (D4). Old env and flags that no longer mean anything are listed in
`conversion-notes.md` and handed to the descriptor owner via @archie.

## 12. Risks

| Risk | Mitigation |
|---|---|
| A `main` change missed by the port | carry ledger, daily merges, cutover gate on zero `open` rows |
| Window too long for 11.4 GB | two-phase copy (7.3), timed in rehearsal |
| Tokens stop verifying | D1 carries keys; golden test old-minted -> new-verified in CI and in 9.1 |
| Outposts disconnect permanently | frozen protocol, golden replay (S11), reconnect watched in 9.1 |
| An agent's in-flight turn lost at stop | outpost sessions survive a hub restart (shim host custody, resume cursors); hub-host `direct` sessions stop as on any hub restart today: listed before the window and resumed after; the window is announced |
| A second process writes the old database during the copy | every opener stopped and checked with `fuser` before the window copy (section 9, step 2) |
| Outposts stranded silently by a protocol mismatch (`hello` closed without an error) | protocol frozen, golden fixtures in CI, every outpost's reconnect checked by name in 9.1 |
| The catalog's git history is written by something outside Tether (`catalog-autocommit` units) | the catalog stays in place and the units are untouched; a tag and a tarball in step zero |
| Hidden state outside `tether.db` | inventory of every store under the data root and the catalog roots |
| Rollback loses post-cutover state | `export-legacy` full sync (section 10), rehearsed after writes of every kind |
| Two trust roots cannot be verified in one auth path | in-process key/deny-list source filed as CW-20261114-0097 before S3 |
| Substrate gap found mid-port | Torque task tagged `substrate-gap`, `cell:mig-tether`; work around where possible; never patch Substrate from the cell |
