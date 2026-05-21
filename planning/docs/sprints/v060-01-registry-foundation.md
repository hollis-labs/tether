# Sprint v060-01 — Registry Foundation

**Epic:** [v0.6 — Federation Directory](../epics/v0.6-federation-directory.md)
**Scope:** Full directory-service surface for kinds `agent` + `project` — CRUD (Register/Lookup/Search/UpdateSelf/Deregister/Sync), HTTP+MCP+CLI parity, `file://` + `cli://` callback resolvers, bootstrap importer, docs+ADR. Ships everything agridd's Phase 3 Stage 3.d.2 needs to integrate without a half-surface bootstrap.
**Target duration:** ~10 business days.
**Transport/shape inheritance:** matches v002-s08 catalog-read-api conventions — UDS default, typed error envelope, GET response shape `{"<kind>s": [...]}`, no pagination, no auth beyond same-host UDS.

**Consumers waiting on this:**
- `agridd` Phase 3 implementer (Stage 3.d.2 — Client implementation). Gated on `notice` from `msg://agent/agent-mux/tether-registry-design` once the surface ships.
- `CW-20260520-0046` (FU-31 in agridd) — peer-link this sprint's task IDs.

**Coordination ref:** Cross-substrate alignment complete 2026-05-20. Thread: `msg://agent/agent-mux/tether-registry-design` ↔ `msg://agent/agent-mux/agridd-keeper`. Do not reopen schema/ownership/interface decisions in this sprint.

---

## Exit criteria

- [ ] Migration `0015_registry.sql` lands; tables hold rows with `mux_instance_id DEFAULT 'agent-mux'` reserved.
- [ ] Mux-minted IDs (`agt_<10alnum>` / `prj_<10alnum>`) generated server-side on every `Register`, returned in the response, never accepted from the caller.
- [ ] `UNIQUE(urn)` enforced; `LookupOrRegister`-style retry-safe semantics work for callers that race.
- [ ] All six ops (Register / Lookup / Search / UpdateSelf / Deregister / Sync) exposed at parity across HTTP, MCP, and CLI.
- [ ] `UpdateSelf` is partial-merge keyed by field name. Arrays support `{mode: append|replace|remove, value: [...]}` and shorthand `[...]` (= REPLACE).
- [ ] `Search` supports filter-and across `{role?, title?, project?, capability?, skill_name?, status?}`; result ordering is alphabetical on `display_name`. No ranking or fuzzy match.
- [ ] `Sync` invokes the row's `callback` and refreshes `cached_payload` + `cached_at`. No-op (with explicit response code) when `callback` is unset.
- [ ] `file://` and `cli://` callback resolvers shipped, with timeout + size limits.
- [ ] Bootstrap importer runs once on first daemon start (idempotent on re-run via `(kind, source_path)` key in `kind_meta`); 22 projects + 8 agents from `~/.tether/catalog/{projects,agents}/*.yaml` land with `file://` callbacks.
- [ ] ADR `0013-registry-directory-service.md` captures the two-store model, the deliberate distinction from v0.2 installer-registry, and the locked interface.
- [ ] `make check` green (`fmt + vet + lint + test-race + vuln`).
- [ ] `notice` envelope sent from `msg://agent/agent-mux/tether-registry-design` to `msg://agent/agent-mux/agridd-keeper` when surface lands.

---

## Decisions locked (don't reopen in this sprint)

- **D1 Two-store model.** Mux registry holds public identity (urn, display_name, title, role, description, capabilities[], skills[], links[], avatar, project, status, last_updated_*). Owning substrate holds ops config. Join on `urn`. `callback` on the row is the URI back to the ops store. Operational config does **not** go in `kind_meta`.
- **D2 IDs.** `agt_<10alnum>` / `prj_<10alnum>`, Stripe-style, Mux-minted on Register, immutable, returned to caller. Callers never supply IDs. Alphabet: lowercase a-z + 0-9, 10 chars. Use `crypto/rand`.
- **D3 URN scheme.** `msg://agent/agent-mux/<id>` only in v1. Schema reserves `mux_instance_id TEXT NOT NULL DEFAULT 'agent-mux'` so future multi-mux federation is a column lookup, not a migration. Search/lookup helpers ignore the column in v1.
- **D4 Global uniqueness.** `UNIQUE(urn)` enforced by the schema, not just at the service layer. Cross-substrate writers all collide cleanly if URNs duplicate.
- **D5 UpdateSelf semantics.** Partial-merge keyed by field name. Arrays: `{skills: [...]}` = REPLACE; `{skills: {mode: "append"|"replace"|"remove", value: [...]}}` explicit. `remove` matches: skills by `name`, capabilities by string equality, links by `(kind, target)` tuple. Empty `value` is a no-op.
- **D6 Search.** Filter-AND across the documented filters, alphabetical on `display_name` ASC. No ranking. No fuzzy. No pagination v1 (registry expected <few thousand rows).
- **D7 Auth.** Same-host UDS trust model. No per-agent tokens. No ACL. If a substrate later needs cross-host token auth, that lands in Sprint v060-02 under its own ADR.
- **D8 Storage.** New tables in `~/.tether/state.db` via migration `0015_registry.sql`. Same DB as `launch_plans` — fewer files, transactional joins available later.
- **D9 Callback transports.** `file://` + `cli://` only in v1. `http://` + `mcp://` → Sprint v060-02. Resolver interface designed to extend without schema change.
- **D10 Bootstrap.** Auto-run on first daemon start. Idempotent via `(kind, source_path)` lookup in `kind_meta`. Re-run = no-op for existing rows. Operators can force-refresh by running `mux registry sync <urn>` per row, or `mux registry bootstrap --force` for the whole catalog.
- **D11 Soft delete.** `Deregister` sets `status = 'deprecated'`. Row stays for audit + foreign-references. No hard delete v1.
- **D12 `kind_meta` discipline.** Kind-specific identity bits only. Project: `{repo_root, tracking_root, knowledge_base_paths[]}` (for display, not for ops). Agent: `{roles[]}` (the secondary roles beyond the primary `role` field). Source-of-truth ops config stays in the YAML behind `callback`.
- **D13 Skills schema.** `{name: string (required), learned_at: RFC3339 (required), via: string (optional), level: string (optional, free-form v1)}`.
- **D14 User-facing terminology.** "Registry" everywhere on user-facing surfaces (HTTP path, MCP tool names, CLI subcommand, ADR). Internal Go package name resolved in Task 1.
- **D15 Cross-kind columns.** `health_status TEXT`, `last_seen_at DATETIME`, `host_address TEXT` are first-class columns on `registry_entries`, nullable, settable via UpdateSelf as scalar fields. Adopted in coordination with cerberus 2026-05-20 — useful across every kind (agent session endpoint, project deploy URL, future service/resource/container health).
- **D16 Link-kind vocabulary.** The `links.kind` column is free-form TEXT for forward compatibility. ADR 0013 documents a blessed v1 vocabulary that consumers should target for cross-substrate discovery: `primary_mailbox`, `team_lead`, `runs_on_host`, `depends_on_service`, `pipeline`, `supervised_by`, `repo`, `dns_zone`, `connector_for`, `requires_secret`. Unknown kinds work without schema change.
- **D17 Secrets factoring.** `requires_secret` is a link kind with opaque-string target v1 (e.g. `op://vault/item`, `secret://owner/id`). Mux holds the reference, not the material. A formal `secret` Mux-minted kind (`sec_<10alnum>`) is deferred to a later sprint, paired with cerberus's 1Password mechanism work (CW-20260519-0027).
- **D18 No raw payload caching.** `registry_entries` does NOT have a `cached_payload_json` column. Sync reads the callback content, extracts identity fields, updates the thin-profile columns, and bumps `cached_at`. Raw payload is discarded. Reason: substrate ops-store files commonly contain plaintext secrets (cerberus YAMLs hold `CLAUDE_CODE_OAUTH_TOKEN` and similar in `resources[].config.env`); caching raw content would leak those into `state.db`. Callers who need full payload read it directly from the callback URI — the source file is the truth.

---

## Tasks

### T-v060-01-01: Schema migration + Go model types + package layout

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, schema, migration, foundation, fu-31, cw-20260520-0046]

#### Problem

No tables exist for the federation directory. No Go types. Existing `internal/registry/` package owns a different concern (launch resolution); naming collision must be resolved before any new code lands.

#### Fix direction

- Resolve package layout. Two options — pick during this task:
  1. Rename existing `internal/registry/` → `internal/launchresolve/` (or similar) and use `internal/registry/` for the new directory service. Higher long-term clarity; requires touching all current call sites (estimate <10).
  2. Keep existing `internal/registry/`; new directory service goes in `internal/directory/`. Zero rename risk; some reader confusion (user-facing surface still says "registry").
  Document choice + rationale at the top of the new package's `doc.go`.
- Write `internal/store/migrations/0015_registry.sql`. Tables:
  - `registry_entries (urn TEXT PK, kind TEXT NOT NULL CHECK(kind IN ('agent','project')), mux_instance_id TEXT NOT NULL DEFAULT 'agent-mux', display_name TEXT NOT NULL, title TEXT, role TEXT, description TEXT, avatar TEXT, project TEXT, status TEXT NOT NULL DEFAULT 'active', callback_json TEXT, cached_at DATETIME, health_status TEXT, last_seen_at DATETIME, host_address TEXT, kind_meta_json TEXT, last_updated_by TEXT, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`. Indexes on `(kind)`, `(kind, status)`, `(kind, project)`, `(kind, role)`. `UNIQUE(urn)` is implicit via PK.
  - `registry_capabilities (urn TEXT NOT NULL, capability TEXT NOT NULL, PRIMARY KEY(urn, capability), FOREIGN KEY(urn) REFERENCES registry_entries(urn) ON DELETE CASCADE)`. Index on `(capability)`.
  - `registry_skills (urn TEXT NOT NULL, name TEXT NOT NULL, learned_at DATETIME NOT NULL, via TEXT, level TEXT, PRIMARY KEY(urn, name), FOREIGN KEY(urn) REFERENCES registry_entries(urn) ON DELETE CASCADE)`. Index on `(name)`.
  - `registry_links (urn TEXT NOT NULL, kind TEXT NOT NULL, target TEXT NOT NULL, PRIMARY KEY(urn, kind, target), FOREIGN KEY(urn) REFERENCES registry_entries(urn) ON DELETE CASCADE)`.
- Add Go types in the new package: `Profile`, `Skill`, `Link`, `Callback`, `Filter`, `ArrayPatch` (the merge-mode wrapper).
- ID generator: `MintAgentURN()` / `MintProjectURN()` using `crypto/rand` over `[a-z0-9]{10}` with collision-retry against the table.

#### Files

- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/store/migrations/0015_registry.sql` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/registry/` or `internal/directory/` — new package skeleton (`doc.go`, `model.go`, `id.go`)
- If renaming: `internal/registry/` → new path (one-line file moves + import updates across the codebase).

#### Acceptance criteria

- [x] Migration applies cleanly on a fresh `~/.tether/state.db` and on an existing one with prior migrations.
- [x] Tables exist with the indexes listed above. FK declarations present in `0015_registry.sql` as schema documentation per ADR 0008. Enforcement deferred; `PRAGMA foreign_keys` NOT flipped on. Go-layer validation at the storage boundary rejects obvious orphans (no `urn` insert into `registry_capabilities` without matching `registry_entries` row). _(Amended 2026-05-20 per agridd-keeper response msg `019e482c-47cb-7213-910e-5485df488032`; full follow-up routed to v060-02.)_
- [x] `MintAgentURN()` / `MintProjectURN()` return `msg://agent/agent-mux/agt_xxxxxxxxxx` / `prj_xxxxxxxxxx`; collision-retry tested (mock the PRNG, force one collision, verify success).
- [x] `make check` green.
- [x] Package-layout decision recorded in `doc.go` with one-paragraph rationale.

#### Scope fences

- Do not implement CRUD or service layer in this task — only schema + types + ID minting.
- Do not touch `internal/registry/` (the launch-resolution layer) beyond the rename if Option 1 is chosen.

---

### T-v060-01-02: Storage layer — CRUD + Search

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, storage, sqlite, foundation]
**depends_on:** T-v060-01-01

#### Problem

Service layer needs typed storage methods. SQLite quirks (transactions, NULL handling, JSON columns, FK cascade) live here, not in the service.

#### Fix direction

- New `storage.go` in the registry package. Methods:
  - `InsertProfile(ctx, profile) error` — INSERT into all four tables in a single transaction.
  - `GetProfile(ctx, urn) (Profile, error)` — JOIN across all four tables; returns wrapped `ErrNotFound` if absent.
  - `UpdateProfileFields(ctx, urn, fields map[string]any) error` — column-level partial update of `registry_entries` only.
  - `ReplaceCapabilities/Skills/Links(ctx, urn, []T) error` — full-replace within transaction.
  - `AppendCapabilities/Skills/Links(ctx, urn, []T) error` — insert-or-ignore.
  - `RemoveCapabilities(ctx, urn, []string) error` / `RemoveSkills(ctx, urn, []name) error` / `RemoveLinks(ctx, urn, []{kind,target}) error`.
  - `SoftDelete(ctx, urn) error` — sets `status='deprecated'`, `updated_at=NOW()`.
  - `Search(ctx, kind, filter) ([]Profile, error)` — builds parameterized SQL from filter, returns ORDER BY display_name ASC.
  - `BumpCachedAt(ctx, urn, at time.Time) error` — used by Sync. Sync also calls the thin-profile column-update methods above; no raw-payload column exists (see D18).

#### Acceptance criteria

- [x] All methods covered by unit tests against an in-memory SQLite.
- [x] Transaction boundaries validated (insert profile + capabilities + skills + links is atomic).
- [x] Search filter combinations tested (each filter alone + all combined).
- [x] FK cascade NOT exercised in v1 (D11 soft-delete only — no hard `DELETE FROM registry_entries`). Storage-layer test confirms `SoftDelete` sets `status='deprecated'` without touching child tables. Cascade-equivalent cleanup (if/when hard-delete lands) is a Go-layer concern handled in a follow-up sprint. _(Amended 2026-05-20 per agridd-keeper response msg `019e482c-47cb-7213-910e-5485df488032`.)_
- [x] `make test-race` green.

#### Scope fences

- No service-layer logic (validation, merge semantics, callback dispatch). Pure data access.
- No HTTP / MCP / CLI surfacing in this task.

---

### T-v060-01-03: Registry service core — Register / Lookup / UpdateSelf / Deregister

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, service, foundation]
**depends_on:** T-v060-01-02

#### Problem

The storage layer is dumb; the service is where validation, ID minting, the partial-merge UpdateSelf semantics, and the URN uniqueness rules live.

#### Fix direction

- New `service.go`. `Service` struct wraps the storage layer.
- `Register(ctx, kind, profile) (Profile, error)`:
  - Reject if caller supplied a urn or id.
  - Validate required fields (`display_name`, `kind`, `status` defaults to `'active'`).
  - Validate `skills[]` entries against D13 (name + learned_at required).
  - Mint URN (`MintAgentURN` / `MintProjectURN`), retry up to 5x on `UNIQUE(urn)` collision.
  - Insert via storage layer.
  - Set `last_updated_by = "<caller-context>"` (sprint v060-01 uses a placeholder string — token-based identity is Sprint v060-02).
- `Lookup(ctx, urn) (Profile, error)` — passthrough + caller-side `errors.Is(err, ErrNotFound)`.
- `UpdateSelf(ctx, urn, patch UpdatePatch) (Profile, error)`:
  - Implement D5 partial-merge. `patch` shape: `{display_name?, title?, role?, description?, avatar?, project?, status?, last_updated_by, capabilities?: ArrayPatch, skills?: ArrayPatch, links?: ArrayPatch, kind_meta?: map}`.
  - Each scalar field present → column update. Each array field present → REPLACE / APPEND / REMOVE per `ArrayPatch.Mode` (default REPLACE for shorthand `[...]`).
  - Always bump `updated_at`. Set `last_updated_by` from `patch.last_updated_by` (required).
  - Wrap whole operation in a transaction.
- `Deregister(ctx, urn) (Profile, error)` — soft-delete via storage; return the now-deprecated profile.

#### Acceptance criteria

- [ ] Table-driven tests cover Register validation (missing fields, caller-supplied URN, malformed skills).
- [ ] Tests cover all UpdateSelf modes for each array field: shorthand REPLACE, explicit REPLACE / APPEND / REMOVE, empty value, conflicting modes.
- [ ] URN collision-retry tested (mocked).
- [ ] Concurrent UpdateSelf on the same URN serializes correctly (no lost updates — last writer wins on each field).
- [ ] `make test-race` green.

#### Scope fences

- No HTTP / MCP / CLI in this task.
- No callback or sync logic in this task — that's T-v060-01-04.
- No bootstrap logic in this task — that's T-v060-01-08.

---

### T-v060-01-04: Sync + callback resolvers (`file://` + `cli://`)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, callback, sync, foundation]
**depends_on:** T-v060-01-03

#### Problem

`Sync(urn)` must fetch fresh content via the row's `callback` and update `cached_payload`. Two transports v1: `file://` (read file from disk) and `cli://` (execute binary, capture stdout). Need a clean resolver interface so v060-02 can drop in `http://` + `mcp://`.

#### Fix direction

- New `callback.go`. Interface:
  ```go
  type Resolver interface {
      Scheme() string
      Resolve(ctx context.Context, target string) ([]byte, error)
  }
  ```
- `FileResolver`: parses `file://<abs-path>`, reads via `os.ReadFile`, enforces a max size (1 MiB), denies symlink escape from a parent-dir allowlist (catalog dirs only).
- `CLIResolver`: parses `cli://<binary>[ <arg>...]`, executes via `exec.CommandContext` with a 5s timeout, captures stdout, enforces 1 MiB output cap.
- `Service.Sync(ctx, urn)`:
  - Lookup the row; if `callback` is null, return `ErrNoCallback` with HTTP 204 No Content semantics.
  - Dispatch to the registered resolver by scheme.
  - Parse the payload (expect JSON or YAML — sniff first byte).
  - Update thin-profile columns from the parsed payload (sync = full refresh of identity). Use the same UpdateSelf merge semantics with full-REPLACE for arrays.
  - Bump `cached_at` via storage `BumpCachedAt`. Raw payload is NOT stored — see D18 for rationale.
  - Return the refreshed profile.
- Register the two resolvers in `NewService(...)`.

#### Acceptance criteria

- [ ] Unit tests for each resolver: happy path, missing file, symlink-escape attempt, oversize payload, CLI timeout, CLI non-zero exit.
- [ ] Sync test with a fixture file callback (round-trip: register → write fixture → sync → assert cached_payload + thin-profile columns updated).
- [ ] Sync test with `cli://echo <json>` style fixture.
- [ ] Sync on a row with no callback returns `ErrNoCallback` not panic.
- [ ] `make test-race` green.

#### Scope fences

- No `http://` or `mcp://` resolvers in this sprint.
- No retry / backoff in resolvers (callers retry).

---

### T-v060-01-05: Search service method + HTTP API on muxd

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, http, api, foundation]
**depends_on:** T-v060-01-03

#### Problem

External callers need HTTP. Six ops × two kinds + Search. Match v002-s08 conventions.

#### Fix direction

- `Service.Search(ctx, kind, filter) ([]Profile, error)` — thin wrapper over storage `Search` plus capability/skill filter join.
- New `internal/api/registry.go`. `registerRegistry(mux, server)` wires:
  - `POST /registry/{kind}` → `handleRegister` (body: profile JSON; response: 201 + minted urn + full profile).
  - `GET /registry/{kind}/{urn}` → `handleLookup` (404 on not-found).
  - `GET /registry/{kind}` → `handleSearch` (query params: `role`, `title`, `project`, `capability`, `skill_name`, `status`).
  - `PATCH /registry/{kind}/{urn}` → `handleUpdateSelf` (body: UpdatePatch JSON).
  - `DELETE /registry/{kind}/{urn}` → `handleDeregister` (returns the deprecated profile).
  - `POST /registry/{kind}/{urn}/sync` → `handleSync` (204 if no callback, 200 + refreshed profile otherwise).
- Reuse the existing error envelope helper. Map service errors: `ErrNotFound` → 404, validation → 400, `ErrNoCallback` → 204, anything else → 500.
- Wire `registerRegistry` from `internal/api/server.go`.
- Extend `internal/client/client.go` with typed `RegistryClient` methods mirroring each endpoint, using the existing `getJSON` / `doJSON` helpers.

#### Files

- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/api/registry.go` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/api/registry_test.go` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/api/server.go` (extend)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/client/client.go` (extend)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/client/registry_client_test.go` (new)

#### Acceptance criteria

- [ ] Each endpoint exercised against `httptest.Server` with a temp-dir SQLite store.
- [ ] Non-allowed methods return `405 method_not_allowed`.
- [ ] Validation errors return `400 invalid_request` with actionable messages.
- [ ] Soft-deleted entries: still appear in `GET /registry/{kind}/{urn}` (with `status: "deprecated"`) but NOT in `GET /registry/{kind}` search results by default. A `status=deprecated` filter brings them back.
- [ ] Client method tests cover happy + 404 + 500 + unreachable for each op.

#### Scope fences

- No auth in this task.
- No pagination / cursor in this sprint.
- No event emission on writes — registry doesn't yet feed the event bus (separate decision for v060-02).

---

### T-v060-01-06: MCP tools (`tether_registry_*`)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, mcp, foundation]
**depends_on:** T-v060-01-03

#### Problem

Agents inside Mux sessions need to register / lookup / search via MCP. Surface parity with HTTP.

#### Fix direction

- Add tools to the existing MCP stdio adapter or proxy aggregation surface (inspect `internal/mcpadapter/` to confirm exact insertion point):
  - `tether_registry_register` — args: `kind`, `profile`. Returns minted urn + full profile.
  - `tether_registry_lookup` — args: `urn`. Returns profile or `ErrNotFound`.
  - `tether_registry_search` — args: `kind`, optional filters. Returns array.
  - `tether_registry_update_self` — args: `urn`, `patch`. Returns refreshed profile.
  - `tether_registry_deregister` — args: `urn`. Returns deprecated profile.
  - `tether_registry_sync` — args: `urn`. Returns refreshed profile or no-op result.
- Each tool dispatches to `Service` directly (no HTTP round-trip from inside the daemon).
- Schemas typed; descriptions explicit about the partial-merge semantics on `update_self`.

#### Acceptance criteria

- [ ] Each tool callable via the MCP adapter against a fixture service.
- [ ] Tool schemas validate (no loose `additionalProperties` on patches).
- [ ] `update_self` tool description explains shorthand vs explicit array mode (so the calling agent understands the merge model).

#### Scope fences

- Don't add tools for kinds other than `agent` / `project` in this sprint.
- Don't auto-register the calling agent's URN-of-caller — `update_self` takes the urn explicitly v1. Identity-of-caller binding is a separate concern.

---

### T-v060-01-07: CLI subcommands (`mux registry ...`)

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [registry, cli, foundation]
**depends_on:** T-v060-01-05

#### Problem

Operators need a CLI to inspect / register / sync from the shell. Mirrors the HTTP surface via the client package.

#### Fix direction

- New `cmd/mux/registry.go`. Subcommands:
  - `mux registry register --kind {agent|project} --file <yaml-or-json> [--print-urn-only]`
  - `mux registry lookup <urn>` (pretty-prints by default; `--json` for machine output)
  - `mux registry search --kind <kind> [--role X --capability Y ...]`
  - `mux registry update-self <urn> --file <patch.yaml>`
  - `mux registry deregister <urn>`
  - `mux registry sync <urn>`
  - `mux registry bootstrap [--force]` (delegates to the bootstrap importer — see T-08)
- All commands route through the typed `RegistryClient`. No filesystem reads bypassing the daemon.
- Output: pretty (default) and `--json` (raw API response).

#### Acceptance criteria

- [ ] Each subcommand has integration tests against a running daemon fixture (existing pattern in `cmd/mux/*_test.go`).
- [ ] `--help` text documents the partial-merge semantics on `update-self`.
- [ ] Exit codes: 0 success, 1 not-found, 2 validation error, 3 daemon-unreachable, 4 internal-error.

#### Scope fences

- No TUI integration (TUI was removed in v005-06; no reintroduction here).
- No interactive prompts.

---

### T-v060-01-08: Bootstrap importer

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, bootstrap, importer, foundation]
**depends_on:** T-v060-01-03, T-v060-01-04

#### Problem

The 22 projects + 8 agents already living in `~/.tether/catalog/{projects,agents}/*.yaml` need to land in the registry on first daemon start. Must be idempotent — re-running daemon doesn't create duplicates.

#### Fix direction

- New `bootstrap.go` in the registry package. Entry point: `BootstrapFromCatalog(ctx, service, catalogRoot string, force bool) (BootstrapReport, error)`.
- For each `agents/*.yaml`:
  - Skip `*.bak-*` files.
  - Parse via `config.Agent` (existing type).
  - Derive thin profile: `display_name` ← `Name`; `role` ← first of `Roles`; `description` ← (none in source, leave empty); `capabilities[]` ← `Skills` (legacy field on `config.Agent`); `kind_meta` ← `{roles: <secondary roles[]>, source_path: <abs-path>}`.
  - `callback = {scheme: "file", target: "file://<abs-path>"}`.
  - Lookup-or-register: check for an existing row where `kind_meta.source_path == <abs-path>`; if found, skip (unless `force=true` — then issue UpdateSelf with the refreshed thin profile).
  - On `force=true`, also call `Sync(urn)` to refresh `cached_payload`.
- Same pattern for `projects/*.yaml`: `display_name` ← `Name`; `role` ← (none in source — leave empty); `kind_meta` ← `{repo_root, tracking_root, knowledge_base, source_path}`; `callback = file://<abs-path>`.
- Daemon startup wires `BootstrapFromCatalog(..., force=false)` after the migrator runs but before HTTP listener binds.
- `mux registry bootstrap --force` re-runs with `force=true` for catalog drift cases.
- `BootstrapReport`: counts (imported, skipped-existing, errors-per-file) for the daemon log.

#### Acceptance criteria

- [ ] Idempotency test: run bootstrap twice; second run reports 0 imported, N skipped-existing.
- [ ] `--force` test: run with --force after modifying a fixture YAML; row's display_name updates.
- [ ] Skip-on-error: a malformed YAML produces an error in the report but does NOT abort the bootstrap of other files.
- [ ] First-daemon-start integration test: fresh DB + populated catalog → expected row count after startup.
- [ ] Backup files (`*.bak-*`) excluded.

#### Scope fences

- Do NOT write the minted URN back into the source YAML in this sprint (write-back to source = Sprint v060-02).
- Do NOT import the other catalog kinds (`providers`, `launches`, `mcp-servers`, `boot-profiles`, `sandbox-profiles`) — agent + project only v1.

---

### T-v060-01-09: ADR 0013 + docs + ship notice

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [registry, adr, docs, ship-notice]
**depends_on:** T-v060-01-05, T-v060-01-06, T-v060-01-07

#### Fix direction

- `docs/adr/0013-registry-directory-service.md`:
  - Context: cross-substrate (agridd FU-31 + cerberus input) need + chrispian's federation framing.
  - Decision: ship Mux-owned directory service with two-store model; opaque IDs; URN scheme; dual-maintenance (callback + UpdateSelf); cross-kind columns (`health_status` / `last_seen_at` / `host_address`); blessed link-kind vocabulary (10 kinds, free-form-extensible — see vocabulary table below); `requires_secret` for secrets-as-link, formal `secret` kind deferred.
  - Consequences: substrates retain ops ownership; Mux owns discovery; raw callback payload is intentionally NOT cached in state.db (D18) — substrate ops-store files often contain plaintext secrets, so Sync refreshes thin-profile columns only and callers read full payload directly from the callback URI; v060-02 picks up cerberus catalog bootstrap + cross-substrate dedup primitive + URN write-back; v060-03 picks up http/mcp callbacks + multi-mux federation.
  - Link-kind vocabulary table (v1, blessed): `primary_mailbox` (any → msg:// URN), `team_lead` (any → agent URN), `runs_on_host` (svc/res/ctr → server URN), `depends_on_service` (svc/res → svc/res URN), `pipeline` (prj/svc → pipeline URN), `supervised_by` (svc/res → literal "launchd"|"systemd"|"dev-session"), `repo` (agt/prj/svc → git remote URL), `dns_zone` (svc/res → domain URN), `connector_for` (svc/res → connector URN), `requires_secret` (svc/res/con → opaque secret reference).
  - Alternatives considered: (a) agridd hosts its own directory (rejected — fragments discovery); (b) extend existing `internal/registry/` launch-resolution package (rejected — different concern); (c) reuse v002-s08 catalog read API (rejected — read-only, file-only, no write/search semantics).
- `docs/api/README.md` — add a `## Registry` section: each endpoint, request/response shapes, partial-merge semantics on PATCH, link to ADR 0013.
- `docs/registry/` (new dir) — `overview.md` summarizing the two-store model + bootstrap behavior for downstream substrate authors.
- After all other tasks land + `make check` green: send `notice` from `msg://agent/agent-mux/tether-registry-design` to `msg://agent/agent-mux/agridd-keeper` with subject `"Sprint v060-01 SHIPPED — Mux registry surface live"` and a brief body covering: prod endpoint summary, bootstrap completed (counts), known caveats. This unblocks agridd Phase 3 Stage 3.d.2.

#### Acceptance criteria

- [ ] ADR 0013 lands, dated, sequential, linked from API docs.
- [ ] API docs cover all six endpoints with example payloads (including a partial-merge PATCH example).
- [ ] `docs/registry/overview.md` exists.
- [ ] Ship notice envelope sent and message_id captured in the sprint close notes.

---

## Review / readiness notes

- **Existing `internal/registry/` package collision.** Decided in Task 1. If renamed, all affected import paths get one-shot updated in the same commit so the codebase compiles continuously.
- **SQLite WAL mode.** State.db already runs in WAL via existing migrator config — no special handling.
- **Bootstrap timing.** Importer runs synchronously during daemon startup, after migrator, before listener binds. If it takes >2 sec on a populated catalog, surface a log line; if >10 sec, that's a follow-up (defer to v060-02 background loading).
- **Test-isolation.** All tests use a temp-dir SQLite + temp catalog dir. No tests mutate `~/.tether/`.
- **Error envelope.** Reuse the seven existing API error codes from ADR 0010 — do not invent new ones.
- **Trust model.** Same-host UDS gates everything. The `last_updated_by` field is informational v1 (caller-supplied string); becomes auth-bound in v060-02.
- **agridd coordination.** Ship-notice is the integration trigger. Do not invite agridd's Phase 3 implementer to start Stage 3.d.2 before all sprint tasks land — partial-surface integration was explicitly rejected in coordination.
- **Cross-substrate project dedup is a v060-02 followup.** Sprint 1's bootstrap imports Tether's catalog only. Cerberus's bootstrap pairs with a dedup primitive (`LookupBy(kind, external_id, substrate?)`) in v060-02 to avoid double-registering projects that both substrates own files for. Sprint 1 does not need to solve this; the importer is idempotent within Tether's source paths.

---

## Done checklist (at sprint close)

- [ ] All nine task acceptance sections ticked.
- [ ] Exit criteria above all ticked.
- [ ] `make check` green.
- [ ] ADR 0013 committed.
- [ ] Branch FF-merged to `main`, branch deleted.
- [ ] Ship notice sent to `msg://agent/agent-mux/agridd-keeper`; message_id recorded here.
- [ ] CW-20260520-0046 (agridd FU-31) notified via peer-link.

---

## Hand-off snippet for the parallel agent

> You're executing Sprint v060-01 — the foundation sprint for the v0.6 Federation Directory epic. Scope is the full directory-service surface (Register / Lookup / Search / UpdateSelf / Deregister / Sync) for `agent` + `project` kinds across HTTP / MCP / CLI, plus `file://` + `cli://` callback resolvers and a bootstrap importer that auto-imports the existing `~/.tether/catalog/{projects,agents}/*.yaml` population. Read the epic at `planning/docs/epics/v0.6-federation-directory.md` and this sprint file end-to-end before starting. Schema, ownership, and all six Q&A decisions are LOCKED — do not reopen. Coordination thread is `msg://agent/agent-mux/tether-registry-design` ↔ `msg://agent/agent-mux/agridd-keeper`. ship-notice is the last task — do not skip; agridd's Phase 3 implementer is gated on it. ADR 0013 captures the rationale. `make check` green is non-negotiable.
