# ADR 0041: Federation Directory Service (Mux Registry)

**Status:** Accepted — 2026-05-20
**Context:** Sprint v060-01 (Registry Foundation) of the v0.6 Federation Directory epic. Coordination thread `msg://agent/agent-mux/tether-registry-design` ↔ `msg://agent/agent-mux/agridd-keeper`; closed 2026-05-20. Gates agridd Phase 3 Stage 3.d.2.
**Extends:** ADR-0023 (Message Routing Contract), ADR-0040 (Messaging Federation Peer Routing).

## Context

The hollis-labs portfolio runs a growing population of substrates — Tether (agents + projects), cerberus (servers + services + resources), agridd (its own agent population), nanite, hadron — each maintaining its own catalog of identities. Cross-substrate coordination today devolves into ad-hoc URN exchange over private messaging threads; an agent in one substrate cannot ask "who else exists, by role" or "what's the messaging URN for `cerberus-registry-design`" without grep'ing source.

Three approaches were considered:

1. **Each substrate hosts its own directory and federates via cross-store queries.** Pros: zero central coupling. Cons: N-way query fan-out; lookups require knowing in advance which substrate owns the identity (a chicken-and-egg failure); no canonical URN — same identity may have multiple representations.

2. **Reuse Tether's existing v002-s08 catalog read API.** Pros: minimal new code. Cons: read-only, file-only, no search semantics; substrates can't write their own identities; doesn't model project-vs-agent kind separation; not extensible to future kinds (service, secret, connector).

3. **Mux hosts a federation directory service — public-identity rows; substrate retains ops config.** Decision below.

## Decision

Adopt option 3. The Mux registry is a small, opinionated identity directory with deliberately narrow scope:

### Two-store model (D1)

The registry holds **public-identity** rows only. Operational configuration — the agent's system prompt, the service's container image, the project's launch profile — stays in the **owning substrate's** ops store. Each registry row carries a `callback` URI pointing back at the substrate's source-of-truth file; the registry's `Sync` operation fetches that file to refresh thin-profile columns. The substrate is the truth; Mux is the discovery surface.

### Opaque, server-minted IDs (D2)

Stripe-style: `agt_<10alnum>` for agents, `prj_<10alnum>` for projects. Lowercase a-z + 0-9, 10 characters, `crypto/rand`-sourced with rejection sampling against the 36-char alphabet for uniform distribution. Callers MUST NOT supply IDs — the server mints them at `Register` time and returns the canonical URN. Collisions are improbable (36^10 ≈ 3.6×10^15 keyspace) but the minter retries up to 5 times against the table; exhaustion surfaces `ErrMintExhausted` so a broken entropy source or malicious caller can't loop forever.

### URN scheme (D3)

`msg://agent/agent-mux/<id>` everywhere in v1. The schema reserves `mux_instance_id TEXT NOT NULL DEFAULT 'agent-mux'` so multi-mux federation is a column lookup, not a migration. The federation router from ADR-0040 already dispatches on the authority segment; the directory's URN scheme is consistent with that contract.

### Partial-merge updates (D5)

`UpdateSelf(urn, patch)` accepts a typed `UpdatePatch`:
- Scalar fields are `*T` pointers — `nil` means no change; non-nil means set (including pointer-to-empty-string = explicit clear).
- Array fields (`capabilities`, `skills`, `links`) accept two wire shapes:
  - Shorthand `[...]` — equivalent to `{mode: "replace", value: [...]}`.
  - Explicit `{mode: "replace"|"append"|"remove", value: [...]}`.
- Empty `value` is always a no-op on every mode.
- `last_updated_by` is required (informational v1; becomes auth-bound in v060-02).

### Search (D6)

Filter-AND across `{role, title, project, capability, skill_name, status}`. Alphabetical on `display_name`. No ranking, no fuzzy match, no pagination (registry expected to hold low thousands of rows). Default excludes status='deprecated'; `status='*'` (`StatusAny`) returns all.

### Same-host UDS trust (D7)

No per-agent tokens; no ACL. `last_updated_by` is a caller-supplied string v1 (informational). Cross-host token auth lands in a future sprint when the use case shows up.

### Storage (D8)

New tables in `~/.tether/state.db` via migration `0015_registry.sql`. Same database as `launch_plans` — fewer files, transactional joins available later. Tables: `registry_entries` (the main row) + `registry_capabilities` + `registry_skills` + `registry_links` (one row per child).

### Callback transports (D9)

`file://` + `cli://` only in v1. `http://` + `mcp://` land in v060-02. The `Resolver` interface is intentionally minimal — schemes needing richer behavior (auth, retries, streaming) wrap a Resolver inside their own type.

### Soft-delete only (D11)

`Deregister` sets `status='deprecated'`. Row stays for audit + foreign-references. No hard delete in v1.

### `kind_meta` discipline (D12)

Kind-specific identity bits only. Project: `{repo_root, tracking_root, knowledge_base_paths}` (for display, not for ops). Agent: `{roles}` (the secondary roles beyond the primary `role` field). Source-of-truth ops config stays in the YAML behind `callback`. **This is load-bearing for v060-02's cerberus importer** — cerberus YAMLs commonly contain plaintext `CLAUDE_CODE_OAUTH_TOKEN` in `resources[].config.env`; the importer's contract is identity-only-fields.

### Skills schema (D13)

`{name: string (required), learned_at: RFC3339 (required), via: string (optional), level: string (optional, free-form v1)}`.

### User-facing terminology (D14)

"Registry" everywhere on user-facing surfaces — HTTP path `/registry/*`, MCP tool prefix `tether_registry_*`, CLI subcommand `mux registry`, this ADR. Internal Go package name aligned in sprint v060-01 Task 1: the prior `internal/registry/` was renamed wholesale to `internal/launchresolve/` (different concern — launch-spec resolution) so the new directory service can own the `registry` name.

### Cross-kind columns (D15)

`health_status TEXT`, `last_seen_at DATETIME`, `host_address TEXT` are first-class columns on `registry_entries`, nullable, settable via `UpdateSelf` as scalar fields. Useful across every kind — agent session endpoint, project deploy URL, future service/resource/container health. Adopted in coordination with cerberus.

### Link-kind vocabulary (D16)

The `links.kind` column is free-form TEXT for forward compatibility. The v1 blessed vocabulary that consumers should target for cross-substrate discovery:

| Kind | Subject | Target |
|---|---|---|
| `primary_mailbox` | any | `msg://` URN |
| `team_lead` | any | agent URN |
| `runs_on_host` | service/resource/container | server URN |
| `depends_on_service` | service/resource | service/resource URN |
| `pipeline` | project/service | pipeline URN |
| `supervised_by` | service/resource | literal `"launchd"`/`"systemd"`/`"dev-session"` |
| `repo` | agent/project/service | git remote URL |
| `dns_zone` | service/resource | domain URN |
| `connector_for` | service/resource | connector URN |
| `requires_secret` | service/resource/connector | opaque secret reference (e.g. `op://vault/item`, `secret://owner/id`) |

Unknown kinds work without schema change; consumers SHOULD only invent new kinds when an existing one doesn't fit.

### Secrets factoring (D17)

`requires_secret` is a link kind with an opaque-string target in v1. Mux holds the reference, not the material. A formal `secret` Mux-minted kind (`sec_<10alnum>`) is deferred to a later sprint, paired with cerberus's 1Password mechanism work (CW-20260519-0027).

### No raw payload caching (D18)

`registry_entries` does NOT have a `cached_payload_json` column. `Sync` reads the callback content, extracts identity fields, updates the thin-profile columns, and bumps `cached_at`. **Raw payload is discarded.** Substrate ops-store files commonly contain plaintext secrets (cerberus YAMLs hold `CLAUDE_CODE_OAUTH_TOKEN` and similar in `resources[].config.env`); caching raw content would leak those into `state.db`. Callers who need full payload read it directly from the callback URI.

## Consequences

- **Substrates retain ops ownership.** Mux owns discovery. The two stores stay separated by the callback boundary.
- **Bootstrap importer is the source-of-truth bridge.** Tether's own catalog YAMLs land via the v060-01 importer; cerberus's catalog lands via v060-02's importer; both follow the identity-only discipline.
- **Sync expects Profile-shape payloads.** A known limitation: the file-callback resolver returns the raw YAML content of e.g. a `config.Agent` file, but `Service.Sync` decodes into `Profile`. The shapes don't match for Tether-native bootstrap YAMLs. v060-01's bootstrap importer works around this by parsing the source files directly (config-package types) and applying via `UpdateSelf` rather than `Sync`. A long-term fix needs either (a) a Profile-shape callback that runs translation per-substrate, or (b) per-scheme decoder registration that maps source shapes to Profile shape. Either lands in v060-02 or v060-03 alongside the `http://` + `mcp://` resolvers.
- **Empty-array clearing is not supported via Sync.** Per D5, empty `value` is a no-op on every mode, so a payload that omits `capabilities` OR explicitly sends `[]` both leave existing children intact. v060-02 / v060-03 may introduce an explicit clear semantic if substrates ask for it.
- **FK enforcement deferred per ADR-0008.** The `0015_registry.sql` migration declares FK references on the three child tables as documentation; PRAGMA foreign_keys is NOT flipped on. Cascade-equivalent cleanup is a Go-layer concern. v060-02 T-v060-02-08 reopens ADR-0008 and flips enforcement globally, paired with the cerberus-bootstrap workload that meaningfully stress-tests cross-substrate FK paths.
- **v060-02 picks up:** cerberus catalog bootstrap + cross-substrate dedup primitive (`LookupBy(kind, external_id, substrate?)`) + URN write-back into source YAMLs + `http://` + `mcp://` callback resolvers + the FK enforcement flip.
- **v060-03 picks up:** multi-mux federation (`mux_instance_id` lookup queries) + cross-host token auth.

## Alternatives Considered

- **(a) agridd hosts its own directory.** Rejected — fragments discovery, repeats the chicken-and-egg lookup problem in another substrate.
- **(b) Extend the existing `internal/registry/` (now `internal/launchresolve/`) launch-resolution package.** Rejected — different concern; the launch-resolution package serves runtime/agent/MCP-server binding for the launch path, not cross-substrate identity discovery.
- **(c) Reuse v002-s08 catalog read API.** Rejected — read-only, file-only, no write/search semantics, doesn't model kind separation.

## References

- Sprint file: `planning/docs/sprints/v060-01-registry-foundation.md`
- Epic file: `planning/docs/epics/v0.6-federation-directory.md`
- ADR-0008: SQLite Foreign-Key Enforcement — Deferred Until Post-Launch
- ADR-0023: Message Routing Contract
- ADR-0040: Messaging Federation — Authority-Routing Peer Configuration
- API docs: `docs/api/README.md` §Registry
- Overview: `docs/registry/overview.md`
