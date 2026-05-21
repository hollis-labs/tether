# Sprint v060-02 — Cross-Substrate Bootstrap + Dedup

**Epic:** [v0.6 — Federation Directory](../epics/v0.6-federation-directory.md)
**Scope:** Cerberus catalog auto-import, cross-substrate dedup primitive (`LookupBy(kind, external_id, substrate?)`), URN write-back to source YAMLs, merge admin for residual duplicates. Closes the loop on the two cerberus follow-ups identified during v060-01 coordination. Federation transport (`http://` / `mcp://` callbacks + multi-mux prep) is a separate sprint — see v060-03.
**Target duration:** ~8 business days.
**Transport/shape inheritance:** matches v060-01 conventions — UDS default, typed error envelope, `{<kind>s: [...]}` response shape on lists.

**Consumers waiting on this:**
- cerberus operator workflows that depend on a registered project URN per cerberus project.
- Anyone who wants a single registry row per logical project (today every project that exists in both Tether and cerberus catalogs would end up duplicated).

**Coordination refs:**
- Cross-substrate alignment: `msg://agent/agent-mux/tether-registry-design` ↔ `msg://agent/agent-mux/cerberus-registry-design`, 2026-05-20.
- Predecessor sprint: [v060-01-registry-foundation.md](v060-01-registry-foundation.md) — ships before this.
- Don't reopen v060-01 D1–D17 in this sprint.

---

## Exit criteria

- [ ] Migration `0016_registry_external_ids.sql` lands. `registry_external_ids` table exists with `(urn, substrate)` as PK and `(substrate, external_id)` lookup index.
- [ ] Tether bootstrap (already shipped) is back-filled to record `(substrate='tether', external_id=<project.id>)` for every imported row.
- [ ] Cerberus bootstrap runs on first daemon start AFTER Tether bootstrap; reads `~/.cerberus/registry.yaml` as the index; for each entry, uses `LookupBy(kind=project, external_id=<owner>, substrate='cerberus')` to dedup against existing rows; on miss with same external_id under a different substrate (i.e. Tether already registered the same logical project), the cerberus side ATTACHES its external_id to the existing URN rather than registering a new row.
- [ ] `LookupBy(kind, external_id, substrate?)` exposed at parity across HTTP (`GET /registry/{kind}?external_id=<id>&substrate=<sub>`), MCP (`tether_registry_lookup_by`), CLI (`mux registry lookup-by`).
- [ ] URN write-back utility adds `registry_urn: <urn>` field idempotently to source YAMLs (both Tether's `~/.tether/catalog/{agents,projects}/*.yaml` and cerberus's `*.cerberus.yaml`). Opt-out flag at the bootstrap call site.
- [ ] Merge admin command `mux registry merge <urn-src> <urn-dst>` consolidates two URNs that turn out to represent the same entity (one-time cleanup for rows created before this sprint shipped).
- [ ] ADR `0014-cross-substrate-dedup.md` captures the dedup model, why external_ids live in a sibling table, and the URN write-back rationale.
- [ ] `make check` green.
- [ ] After ship: post-bootstrap row count audit shows ~26 unique projects (down from 37 if both substrates had bootstrapped independently into separate URNs).

---

## Decisions locked

- **D1 `external_id` lives in a sibling table.** `registry_external_ids(urn, substrate, external_id)`, PK `(urn, substrate)`, index `(substrate, external_id)`. One substrate per URN; substrate can attach external_ids to multiple URNs (different rows). Same external_id may legitimately collide across kinds (e.g. cerberus has both a project `torque` and a resource `torque-api-service`) — uniqueness is enforced per-kind at the service layer, not in the schema.
- **D2 Bootstrap ordering is Tether-first, deterministic.** Tether bootstrap (already in v060-01) runs first on daemon start. Cerberus bootstrap runs second. URNs are random and immutable, so ordering doesn't change identity — Tether-first is convention for predictability. Substrates that bootstrap after cerberus follow the same `LookupBy → attach OR register` pattern.
- **D3 URN write-back is opt-in via bootstrap flag, default ENABLED for local-file catalogs.** Tether's `~/.tether/catalog/*.yaml` and cerberus's `*.cerberus.yaml` both get a `registry_urn: <urn>` field written at the top level on first import. Idempotent — re-running doesn't duplicate the line. Substrates can disable via `--no-write-back` on `mux registry bootstrap`.
- **D4 Cerberus index source.** Importer reads `~/.cerberus/registry.yaml` for the list of catalog files (the cerberus-side index), NOT a directory walk of `~/.cerberus/projects/`. The registry.yaml index catches out-of-tree catalog files (e.g. `~/dev/hollis-labs/apps/agridd/.cerberus.yaml`).
- **D5 Importer scope discipline.** The cerberus importer extracts ONLY identity fields: `project.id` → external_id; `project.name` → display_name; kind_meta.cerberus = `{namespace, source_path, resources_count}`. It does NOT copy `resources[]`, `config`, `env`, or any other operational content. cerberus YAMLs commonly contain secrets in env blocks (e.g. `CLAUDE_CODE_OAUTH_TOKEN`); the importer never touches those. Sync (from v060-01) inherits this discipline — see open question on `cached_payload_json` in the readiness notes.
- **D6 `LookupBy` is a query helper, not a new CRUD op.** Surfaces as additional query params on the existing search endpoint (`GET /registry/{kind}?external_id=<id>&substrate=<sub>`). MCP gets a dedicated `tether_registry_lookup_by` tool since its semantics differ from generic search (returns 0 or 1 row, not a list). CLI gets `mux registry lookup-by --kind <k> --external-id <id> [--substrate <sub>]`.
- **D7 Merge command is manual, no auto-merge.** `mux registry merge <urn-src> <urn-dst>` moves external_ids, kind_meta, links, capabilities, skills from src to dst (with collision rules: src loses on scalar conflicts; arrays union); src gets soft-deleted with `status='merged'` and a `merged_into` pointer. Used once after upgrading from v060-01 to v060-02 to clean up any residual duplicates from the brief window when both substrates had bootstrapped without dedup.
- **D8 YAML write-back uses comment-preserving YAML.** Use `gopkg.in/yaml.v3` round-trip with `yaml.Node`. If round-trip would lose existing comments or formatting, the write-back falls back to "append to end of file" with a `# registry_urn — added by mux registry bootstrap` comment. Operators may manually relocate the line.

---

## Tasks

### T-v060-02-01: Migration 0016 + `external_id` types

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, schema, migration, dedup]

#### Problem

No place to store substrate ↔ external_id mappings. Dedup primitive can't be built without the column.

#### Fix direction

- New migration `internal/store/migrations/0016_registry_external_ids.sql`:
  ```sql
  CREATE TABLE registry_external_ids (
    urn         TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    substrate   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    attached_at DATETIME NOT NULL,
    PRIMARY KEY (urn, substrate)
  );
  CREATE INDEX idx_extid_lookup ON registry_external_ids (substrate, external_id);
  ```
- Add Go types in the registry package: `ExternalID{Substrate, ExternalID, AttachedAt}`. Methods on `Profile` for `WithExternalID(...)`, `ExternalIDFor(substrate)`.
- Extend `registry_entries` with one new column for the merge-soft-delete trail:
  ```sql
  ALTER TABLE registry_entries ADD COLUMN merged_into TEXT REFERENCES registry_entries(urn) ON DELETE SET NULL;
  ```
  Nullable. Set by the merge command (T-07). When non-null and `status='merged'`, callers should treat the row as a tombstone pointing at the canonical URN.

#### Acceptance criteria

- [ ] Migration applies cleanly on a fresh DB and on a DB with 0015 already applied.
- [ ] FK cascade verified (deleting a registry_entries row removes its external_ids).
- [ ] `merged_into` column nullable, defaulted NULL.
- [ ] `make check` green.

---

### T-v060-02-02: Storage + Service: `LookupBy` primitive + `AttachExternalID` + `BackfillTetherExternalIDs`

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, storage, service, dedup]
**depends_on:** T-v060-02-01

#### Fix direction

- New storage methods:
  - `LookupExternalIDsForURN(ctx, urn) ([]ExternalID, error)`
  - `LookupURNByExternalID(ctx, kind, external_id, substrate string) (urn string, ok bool, err error)` — when substrate is "", matches across all substrates and returns the first hit (ordered by attached_at ASC).
  - `AttachExternalID(ctx, urn, substrate, external_id) error` — insert-or-error on PK collision (substrate already has an external_id for this URN — that's a logic error in the caller).
  - `DetachExternalID(ctx, urn, substrate) error` — used by the merge command.
- New service method:
  - `Service.LookupBy(ctx, kind, external_id, substrate string) (Profile, error)` — wraps storage `LookupURNByExternalID` then `GetProfile`.
- New service method:
  - `Service.AttachExternalID(ctx, urn, substrate, external_id string) error` — validates `(substrate, external_id)` doesn't already map to a DIFFERENT urn of the same kind (would be a registration mistake).
- One-shot back-fill helper `BackfillTetherExternalIDs(ctx, service, catalogRoot string) (int, error)` — walks Tether's bootstrap-imported rows (identified by `kind_meta.source_path` starting with `<catalogRoot>/{agents,projects}/`), reads each source YAML to get the project's slug/id, calls `AttachExternalID(urn, 'tether', <slug>)`. Run once during daemon startup AFTER 0016 migration applies, BEFORE cerberus bootstrap runs.

#### Acceptance criteria

- [ ] `LookupBy` returns the right URN for known (kind, external_id, substrate) tuples.
- [ ] `LookupBy(substrate="")` returns the earliest-attached match across substrates.
- [ ] `AttachExternalID` rejects an attempt to map (substrate, external_id) to two different URNs of the same kind; allows it across different kinds (project `torque` and resource `torque-api-service`).
- [ ] `BackfillTetherExternalIDs` is idempotent — re-run produces no new attachments, no errors.
- [ ] `make test-race` green.

---

### T-v060-02-03: HTTP + MCP + CLI surface for `LookupBy`

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, http, mcp, cli, dedup]
**depends_on:** T-v060-02-02

#### Fix direction

- HTTP: extend the existing `GET /registry/{kind}` handler. New query params:
  - `external_id=<id>` — when set, returns at most one row.
  - `substrate=<sub>` — optional, narrows the lookup to one substrate. When `external_id` is set but `substrate` is not, matches across substrates.
  - When `external_id` is set and no row matches, return `404 not_found` (not an empty list — semantics differ from search).
- MCP: new tool `tether_registry_lookup_by` with args `{kind, external_id, substrate?}`. Returns one profile or `ErrNotFound`.
- CLI: new subcommand `mux registry lookup-by --kind <k> --external-id <id> [--substrate <sub>]`. Pretty-prints by default; `--json` for machine.
- All three surfaces share the service-layer `LookupBy`.

#### Acceptance criteria

- [ ] HTTP `?external_id=<id>` returns single row, not list shape (response shape: `{<kind>: {...}}` not `{<kind>s: [...]}`). Match the existing GET-by-urn shape.
- [ ] HTTP `404 not_found` on no-match.
- [ ] MCP tool description explicitly says "returns 0 or 1 — use this when you have a substrate's local ID and want to find the URN".
- [ ] CLI exit codes match v060-01 conventions (0=found, 1=not-found, 2=validation, 3=unreachable).

---

### T-v060-02-04: Cerberus catalog importer + `cli://mux registry bootstrap --substrate cerberus`

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, bootstrap, cerberus, importer]
**depends_on:** T-v060-02-03, T-v060-02-02

#### Problem

Cerberus's 15 catalog entries (per `~/.cerberus/registry.yaml` as of 2026-05-19) need to land in the registry, deduping against the 11 that overlap with Tether's catalog.

#### Fix direction

- New file `bootstrap_cerberus.go` in the registry package.
- Entry point `BootstrapFromCerberus(ctx, service, cerberusHome string, force bool) (BootstrapReport, error)`. Default `cerberusHome` = `~/.cerberus`.
- Read `<cerberusHome>/registry.yaml`. Schema (observed 2026-05-19):
  ```yaml
  version: 1
  entries:
    - owner: <slug>           # → external_id
      namespace: local        # → kind_meta.cerberus.namespace
      path: <abs-path>        # → kind_meta.cerberus.source_path + callback target
      kind: cerberus-project/v1
      registered_at: <ts>     # → kind_meta.cerberus.cerberus_registered_at
  ```
- For each entry:
  1. Skip if `kind` != `cerberus-project/v1` (forward-compat for future cerberus types).
  2. Read the target file. Parse `project: {id, name}` only — do NOT touch `resources[]`.
  3. Look up by `(kind=project, external_id=<owner>, substrate='cerberus')`. If found, skip (or re-Sync if `--force`).
  4. Look up by `(kind=project, external_id=<owner>, substrate='')`. If found via a DIFFERENT substrate (e.g. Tether), call `AttachExternalID(<urn>, 'cerberus', <owner>)` to add the cerberus alias to the existing row. Optionally `UpdateSelf` with cerberus-specific kind_meta fields under `kind_meta.cerberus`.
  5. Otherwise: `Register({kind: 'project', display_name: <project.name>, kind_meta: {cerberus: {namespace, source_path, resources_count: len(resources)}}, callback: {scheme: 'file', target: 'file://<abs-path>'}})`. Then `AttachExternalID(<minted-urn>, 'cerberus', <owner>)`.
- Daemon startup ordering: 0016 migration → Tether external_id backfill → Tether bootstrap (no-op for already-imported rows) → cerberus bootstrap.
- `mux registry bootstrap --substrate cerberus [--force]` re-runs cerberus side manually.

#### Acceptance criteria

- [ ] Fresh-DB + fresh-Tether-bootstrap + then cerberus-bootstrap: 11 overlapping projects attach cerberus external_id to existing Tether-imported URN (one row each, two external_ids); 4 cerberus-only projects (infrastructure, tachyon, agridd, tangent) register new URNs.
- [ ] Idempotent: second cerberus-bootstrap run → 0 imported, 0 attached, 15 skipped.
- [ ] `--force` re-Syncs every cerberus row without re-registering.
- [ ] Skip on malformed YAML (logs the file path + error, continues).
- [ ] Importer does NOT copy `resources[].config.env` or any other non-identity field into the registry. Verified by inspecting kind_meta after a test run on a fixture containing fake secret env vars.

#### Scope fences

- DO NOT import cerberus `resources[]` (services/processes) as `service`/`resource` rows — those kinds don't exist until v060-04.
- DO NOT touch cerberus's `~/.cerberus/registry.yaml` from the importer (cerberus owns that file; only cerberus mutates it).
- DO NOT copy env-block contents, OAuth tokens, or any `config.env.*` fields under any circumstances.

---

### T-v060-02-05: URN write-back utility

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [registry, bootstrap, write-back]
**depends_on:** T-v060-02-04

#### Problem

Source YAMLs don't currently know their own URN, so a substrate that moves a YAML file to a new path or rebuilds its catalog index has to re-derive identity. Write the URN back into the source so the file becomes self-identifying.

#### Fix direction

- New helper `WriteURNBack(ctx, path string, urn string) error`.
- Uses `gopkg.in/yaml.v3` `yaml.Node` round-trip to preserve comments + key ordering.
- Inserts `registry_urn: <urn>` as a top-level key if not already present.
- Idempotent: if the file already has `registry_urn: <urn>`, no-op. If it has `registry_urn: <different-urn>`, log a WARNING and skip (caller's call whether to override; default safe).
- Falls back to append-at-end-of-file with a `# registry_urn — added by mux registry bootstrap` comment if YAML round-trip fails (preserves operator's ability to relocate manually).
- Called by both Tether and cerberus bootstrap importers when the relevant `--write-back` flag is on (default true).
- Future readers of the YAML (Tether's launch resolver, cerberus's connector, etc.) can read `registry_urn` directly if they care.

#### Acceptance criteria

- [ ] Round-trip preserves YAML comments + key ordering in the common case.
- [ ] Idempotent: re-run does not duplicate the key.
- [ ] WARNs on URN mismatch; does not silently overwrite.
- [ ] Fallback append mode tested.
- [ ] No regressions in YAML parsing of files that have had write-back applied (Tether's existing config loader still reads them fine).

#### Scope fences

- Don't write-back to YAMLs outside `~/.tether/catalog/` or the cerberus-registered paths.
- Don't write-back to backup files (`*.bak-*`).

---

### T-v060-02-06: Merge admin command — `mux registry merge <urn-src> <urn-dst>`

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [registry, admin, cli, merge]
**depends_on:** T-v060-02-04

#### Problem

If both substrates bootstrap independently before this sprint lands (or if a race condition produces duplicates later), operators need a way to consolidate two URNs that represent the same logical entity.

#### Fix direction

- New service method `Service.Merge(ctx, urnSrc, urnDst string) (mergedProfile, error)`:
  - Validate both exist, same kind.
  - Move `external_ids` from src → dst (skip any that would collide).
  - Move `kind_meta.<key>` from src → dst (only keys not already present in dst; on collision, prefer dst).
  - Move `links`, `capabilities`, `skills`: union (no duplicates).
  - Set src `status='merged'`, `merged_into=<urnDst>`, `updated_at=NOW()`.
  - Bump dst `updated_at`.
- CLI: `mux registry merge <urn-src> <urn-dst> [--dry-run]` — prints the merge plan, executes on confirmation (or `--yes`).
- HTTP: `POST /registry/{kind}/{urn-src}/merge` body `{into: "<urn-dst>"}` — same semantics.
- MCP: NO mcp tool for merge — admin-only operation; CLI/HTTP only.

#### Acceptance criteria

- [ ] Merge moves external_ids correctly.
- [ ] Merge handles kind_meta collisions (dst wins).
- [ ] Merged src row remains queryable via `Lookup(urn-src)` but is excluded from `Search` results by default.
- [ ] `merged_into` is set on src; reverse traversal (`LookupChain`) finds the canonical URN.
- [ ] `--dry-run` produces output without mutation.

#### Scope fences

- No auto-merge during bootstrap.
- No "split" command (reverse of merge). If needed later, separate sprint.

---

### T-v060-02-07: ADR 0014 + docs

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [registry, adr, docs]
**depends_on:** T-v060-02-03, T-v060-02-04, T-v060-02-05, T-v060-02-06

#### Fix direction

- `docs/adr/0014-cross-substrate-dedup.md`:
  - Context: every substrate maintains its own catalog of projects; Tether (22 projects) + cerberus (15 projects) overlap on 11; future substrates will overlap further; without a dedup primitive the registry becomes useless as a discovery surface.
  - Decision: substrates supply opaque external_ids per kind; Mux stores them in a sibling table; `LookupBy(kind, external_id, substrate?)` is the dedup primitive; bootstrap importers use it before Register; URN write-back makes the source YAML self-identifying.
  - Consequences: substrates can rename their internal slugs without breaking registry identity (URN is stable, external_id changes); future substrates federate by adding their external_id to existing URNs; cross-substrate consumers can search by either URN or `(substrate, external_id)`.
  - Alternatives considered: (a) deterministic URN derivation from slug (rejected — breaks Stripe-style opacity; sketched but flagged by cerberus as opaque-breaking); (b) global slug uniqueness (rejected — substrates can't be forced to coordinate slug namespaces); (c) accept duplicates + add merge later (rejected — duplicate rows pollute discovery from day 1).
- Extend `docs/api/README.md` `## Registry` section with `LookupBy` documentation + the dedup model overview.
- Extend `docs/registry/overview.md` (created in v060-01) with the cross-substrate section: importer ordering, URN write-back, external_id semantics.

#### Acceptance criteria

- [ ] ADR 0014 lands, dated, sequential, linked from API + registry docs.
- [ ] Vocabulary aligned between ADR 0013 + 0014 (consistent terminology for `external_id`, `substrate`, `URN`).

---

### T-v060-02-08: Ship notice to cerberus + cross-substrate audit

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [registry, ship-notice]
**depends_on:** T-v060-02-04, T-v060-02-07

#### Fix direction

- After all tasks land + `make check` green: send `notice` from `msg://agent/agent-mux/tether-registry-design` to `msg://agent/agent-mux/cerberus-registry-design` with subject `"Sprint v060-02 SHIPPED — cerberus bootstrap + dedup live"`. Body: endpoint summary, row-count audit (expect ~26 unique projects after both bootstraps complete), known caveats, the cerberus side can now rely on `LookupBy` for any cross-substrate lookup pattern.
- Also send `notice` to `msg://agent/agent-mux/agridd-keeper` informing them that cross-substrate dedup is live; if agridd ever starts registering projects (currently they only register agents), the same primitive applies.

#### Acceptance criteria

- [ ] Both notices sent.
- [ ] Row-count audit script (one-shot SQL or `mux registry stats`) prints: total registry_entries, unique-by-(kind), unique-projects-by-external_id-set, count of rows with >1 external_id (the federation success metric).

---

## Review / readiness notes

- **Cached payload security concern was raised during this sprint's design and resolved in v060-01 D18.** Tether v060-01 will NOT have a `cached_payload_json` column. Sync refreshes thin-profile columns only and bumps `cached_at`. Substrate ops-store files (cerberus YAMLs commonly contain plaintext `CLAUDE_CODE_OAUTH_TOKEN` in `resources[].config.env`) are read by callers directly via the callback URI when needed. The cerberus importer in T-v060-02-04 follows the same discipline: identity fields only, never env or secrets.
- **Tether external_id backfill MUST run before cerberus bootstrap on first daemon start of v060-02.** Otherwise the LookupBy(`substrate=''`) lookup misses Tether's prior imports and cerberus re-registers everything as new URNs.
- **agridd's project registrations** are out-of-substrate (agridd doesn't register projects today, only agents). If they ever do, they'll use the same `LookupBy` primitive — no special-casing needed.
- **YAML write-back is conservative.** Mismatched URN → WARN + skip. Operators are in the loop for ambiguous cases.
- **Cerberus `resources[]` are explicitly NOT imported.** They become `service` / `resource` rows in v060-04, not now.
- **No new MCP tools beyond `tether_registry_lookup_by`.** No `merge` via MCP (admin-only).

---

## Done checklist (at sprint close)

- [ ] All eight task acceptance sections ticked.
- [ ] Exit criteria above all ticked.
- [ ] `make check` green.
- [ ] ADR 0014 committed.
- [ ] Branch FF-merged to `main`, branch deleted.
- [ ] Ship notices sent to cerberus + agridd; message_ids recorded.
- [ ] Cross-substrate audit run: unique-project count, federation metric (# of rows with >1 external_id).

---

## Hand-off snippet for the parallel agent

> You're executing Sprint v060-02 — cross-substrate bootstrap + dedup, the follow-up to v060-01. Scope is the cerberus catalog importer, the `LookupBy(kind, external_id, substrate?)` dedup primitive, URN write-back to source YAMLs, and a merge admin command for residual duplicates. v060-01 must be shipped first; do not reopen v060-01 D1–D17. Coordination thread: `msg://agent/agent-mux/tether-registry-design` ↔ `msg://agent/agent-mux/cerberus-registry-design`. Read the epic at `planning/docs/epics/v0.6-federation-directory.md` and this sprint file end-to-end before starting. The importer scope discipline (no env, no resources, no secrets — only project identity) is non-negotiable; cerberus YAMLs commonly contain plaintext OAuth tokens that MUST stay in the source file, never in the registry. `make check` green is non-negotiable. Ship notices to cerberus + agridd are the last step.
