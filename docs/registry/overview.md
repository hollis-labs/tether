# Federation Directory — Integration Guide

The Mux registry is Tether's federation directory service: cross-substrate identity discovery for agents and projects (v060-01 scope). Substrates retain operational ownership of their files; Mux owns the public-identity surface used to find them.

This doc is the integration guide for substrate authors. The full architecture rationale lives in [ADR 0041](../adr/0041-registry-directory-service.md) and [ADR 0043](../adr/0043-cross-substrate-dedup.md); the API reference is in [docs/api/README.md §Registry](../api/README.md#registry).

## Mental model

```
┌─────────────┐         ┌─────────────────┐         ┌──────────────────┐
│ caller      │  POST   │ Mux registry    │  Sync   │ substrate's      │
│ (any agent) │ ──────► │ (this database) │ ──────► │ ops store        │
│             │  GET    │                 │  via    │ (the substrate's │
│             │  PATCH  │  thin profile   │  callback│  YAML or CLI)    │
│             │  DELETE │  + callback URI │         │                  │
└─────────────┘         └─────────────────┘         └──────────────────┘

                                        │
                                        └─► substrate's secrets STAY
                                            in the ops store — never
                                            cached in the registry.
                                            (D18)
```

The registry stores **identity** (URN, display name, role, capabilities, skills, links, status, the callback that points at the source file). It does NOT store **ops config** (service ports, agent prompts, container images, env, secrets). Those live in the owning substrate's files; the callback is how Mux gets a fresh view of identity when those files change.

## URN shape

`msg://agent/agent-mux/<id>` everywhere in v1. The third segment is the Mux instance ID — reserved as a column (`mux_instance_id`) so multi-mux federation is a future column lookup, not a schema migration.

IDs are Stripe-style opaque tokens minted by the server:
- `agt_<10alnum>` for agents
- `prj_<10alnum>` for projects

Callers **never** supply IDs. Callers receive the minted URN in the `Register` response and use it for subsequent operations.

## The two-store contract

Each registry row carries a `callback` URI:

```json
{
  "callback": {"scheme": "file", "target": "file:///Users/chrispian/.tether/catalog/agents/reviewer.yaml"}
}
```

When a caller invokes `Sync(urn)`, Mux:

1. Dispatches to the resolver for the URI's scheme (`file://` and `cli://` in v1; `http://` and `mcp://` in v060-02).
2. Fetches the substrate's current content.
3. Extracts identity fields (display name, role, capabilities, etc.).
4. Updates the thin-profile columns + bumps `cached_at`.

**Raw payload is NEVER stored** (D18 — substrate ops-store files often contain plaintext secrets like `CLAUDE_CODE_OAUTH_TOKEN`; caching would leak them into Mux's `state.db`). Callers who need the full payload read it directly from the callback URI.

### Sync currently expects Profile-shape payloads

This is a v060-01 limitation worth flagging:

`Service.Sync` decodes the callback response as a `registry.Profile` (JSON or YAML; YAML routes through a JSON re-marshal so the registry types' `json:` tags drive field mapping). But Tether's own bootstrap YAMLs are `config.Agent` / `config.Project` shape, NOT `Profile` shape (e.g. `config.Agent.Skills` is `[]string`, while `Profile.Skills` is `[]Skill`).

For Tether's first-party catalog, the bootstrap importer works around this by parsing the source files directly (config-package types) and applying via `UpdateSelf` rather than `Sync`. Substrates whose ops-store files DON'T match Profile-shape have two options in v1:

1. **Emit Profile-shape via `cli://` callback.** Point the callback at a translation binary that reads the ops-store file and emits Profile-shape JSON on stdout. Example: `cli://my-substrate registry-render <ops-store-path>`.
2. **Update identity columns explicitly via `UpdateSelf`.** Don't rely on Sync; the substrate writes a small process that watches its ops-store files and PATCHes the registry directly.

A long-term fix lands in v060-02 or v060-03: per-scheme decoder registration that maps source shapes to Profile shape.

## Lifecycle

### Register

POST a Profile (without urn/kind/timestamps). Mux mints the URN and inserts. Response is the canonical Profile (with all defaults applied).

### Update

PATCH partial-merge per the D5 contract:

- Scalar pointer fields: `nil` = no change, non-nil = set (including pointer-to-empty-string = explicit clear).
- Array fields: two wire shapes accepted:
  - Shorthand: `"capabilities": ["a", "b"]` — equivalent to REPLACE.
  - Explicit: `"capabilities": {"mode": "replace"|"append"|"remove", "value": [...]}`.
- Empty `value` is a no-op on every mode (a known limitation that prevents clearing arrays via Sync — see ADR 0041's Consequences section).
- `last_updated_by` is required (informational in v1; auth-bound in v060-02).

### Deregister

DELETE soft-deletes — `status` flips to `deprecated`, child tables are NOT touched. Soft-deleted rows are still returned by direct URN lookup; they're excluded from default Search unless the caller passes `status=deprecated` or `status=*`.

### Sync

POST `/registry/{kind}/{urn}/sync` — refreshes thin-profile columns from the callback URI. Returns 204 if the row has no callback; otherwise 200 + refreshed Profile.

## Search

`GET /registry/{kind}` with optional query parameters:

| Param | Meaning |
|---|---|
| `role` | exact match on `role` column |
| `title` | exact match on `title` |
| `project` | exact match on `project` |
| `capability` | row has this capability |
| `skill_name` | row has a skill with this name |
| `status` | `active` (default) / `deprecated` / `*` (all) |

All filters AND together. Results are alphabetical by `display_name`. No pagination v1 (the registry is expected to hold low thousands of rows).

## Cross-substrate dedup

v060-02 adds substrate-local identifier attachments:

```json
{
  "external_ids": [
    {"substrate": "tether", "external_id": "clockwork"},
    {"substrate": "cerberus", "external_id": "clockwork"}
  ]
}
```

These attachments are the dedup primitive. Callers resolve them with:

- `GET /registry/{kind}?external_id=<id>&substrate=<sub>`
- `tether_registry_lookup_by`
- `mux registry lookup-by --kind <k> --external-id <id> [--substrate <sub>]`

Bootstrap ordering is deterministic:

1. Tether catalog bootstrap
2. Tether external-id backfill
3. Cerberus index bootstrap

That ordering ensures Cerberus can attach its external ID onto an existing
Tether-imported project row instead of minting a duplicate URN.

### URN write-back

Supported local-file bootstraps write `registry_urn: <urn>` back into the
source YAML so the file carries its shared-directory identity directly. The
write-back is idempotent and conservative: if the file already has a different
`registry_urn`, Tether warns and leaves it unchanged.

## Link-kind vocabulary

The `links.kind` column is free-form text (D16) — substrates can invent kinds without a schema change. The v1 blessed vocabulary that consumers SHOULD target for cross-substrate discovery:

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
| `requires_secret` | service/resource/connector | opaque secret reference |

## Bootstrap importer

The daemon auto-runs `BootstrapFromCatalog(force=false)` at startup to land `~/.tether/catalog/{agents,projects}/*.yaml` files as registry rows. Idempotent — re-runs skip existing rows (matched by `callback.target`).

Operators apply catalog drift via:

```bash
mux registry bootstrap --force
```

The `--force` flag re-applies the YAML's current content to existing rows via `UpdateSelf` and bumps `cached_at`.

### Importer-scope discipline

This is **load-bearing for v060-02**. Tether's first-party catalog YAMLs don't carry secrets, but cerberus's do (`CLAUDE_CODE_OAUTH_TOKEN` in `resources[].config.env`). The importer's contract is:

- Identity-only fields go into the registry.
- `resources[]`, `config`, `env`, operational secrets — **none of those touch the registry**.
- The source file remains the truth; the callback URI lets callers read it directly if they need ops config (which they probably shouldn't, from a different substrate).

Substrate authors writing future importers should treat the cerberus discipline as the general rule, even when their own files don't carry secrets today.

## Surfaces

Six operations × three transports = the full v060-01 surface.

| Op | HTTP | MCP | CLI |
|---|---|---|---|
| Register | `POST /registry/{kind}` | `tether_registry_register` | `mux registry register --kind X --file Y` |
| Lookup | `GET /registry/{kind}/{urn}` | `tether_registry_lookup` | `mux registry lookup <urn>` |
| Search | `GET /registry/{kind}` | `tether_registry_search` | `mux registry search --kind X ...` |
| UpdateSelf | `PATCH /registry/{kind}/{urn}` | `tether_registry_update_self` | `mux registry update-self <urn> --file Y` |
| Deregister | `DELETE /registry/{kind}/{urn}` | `tether_registry_deregister` | `mux registry deregister <urn>` |
| Sync | `POST /registry/{kind}/{urn}/sync` | `tether_registry_sync` | `mux registry sync <urn>` |

Plus the bootstrap operation: `POST /registry/bootstrap?force=true&substrate=tether|cerberus` (HTTP) / `mux registry bootstrap [--force] [--substrate tether|cerberus]` (CLI). No MCP tool for bootstrap — it's an operator concern, not an agent-loop concern.

## What's next (v060-02 + v060-03)

- v060-02: cerberus catalog importer; cross-substrate dedup primitive (`LookupBy(kind, external_id, substrate?)`); URN write-back into source YAMLs; `http://` + `mcp://` callback resolvers; FK enforcement flipped on globally (ADR-0008 reopen).
- v060-03: multi-mux federation (`mux_instance_id` lookup); cross-host token auth; richer payload-translation seam so `Sync` works for non-Profile-shape source files.
