# Shared Project Federation Contract

The **Shared Project** federation surface establishes a single canonical identity
for each project across disparate agent tooling substrates (`tether`, `torque`,
`cerberus`, etc.).

Settled design record: `CW-20260912-0075`.
Implementation stages:
- **C1** (`CW-20260912-0094`): Derived vs. authored field split and per-field provenance.
- **C2** (`CW-20260912-0095`): Callback-backed derivation, authored protection, and identity-keyed idempotency.
- **C3** (`CW-20260912-0096`): Onboarding/lookup surfaces, scope gating, and the `torque` substrate.

---

## Architectural Boundary & Ownership Split

Per design record `CW-20260912-0075`:

| Role | Owner | Responsibility |
|---|---|---|
| **Store & Contract** | **Tether** (`muxd`) | Enforces profile schema, mints canonical URNs, manages cross-substrate external ID mappings, executes callback syncs, and preserves authored metadata. **Tether never calls external APIs** (no Torque/Cerberus/Tesseract REST calls). |
| **Process & Orchestration** | **agent-setup** | Owns onboarding workflows, guides human/agent onboarding decisions, and supplies explicit cross-substrate ID mappings. |
| **Substrates** | **Torque / Cerberus / etc.** | Each substrate mints and owns its own internal IDs (e.g. Torque `PRJ-xxxx`, Cerberus site name, GitHub repo). |

---

## Surface Reference

The shared project federation surface is exposed symmetrically across HTTP, MCP, and CLI:

```
POST   /registry/projects                               # Register (onboard)
GET    /registry/projects/{urn}                         # Lookup (resolve)
GET    /registry/projects?external_id={id}&substrate={s} # LookupBy (reverse lookup)
GET    /registry/projects?tag={t}&role={r}              # Search
PATCH  /registry/projects/{urn}                         # UpdateSelf (partial update)
DELETE /registry/projects/{urn}                         # Deregister (soft-delete)
POST   /registry/projects/{urn}/sync                    # Sync (callback refresh)
POST   /registry/projects/{urn}/merge                   # Merge (consolidate into destination)

tether_registry_register       kind="project" profile={...}
tether_registry_lookup         urn= [include=]
tether_registry_lookup_by      kind="project" external_id= [substrate=] [include=]
tether_registry_search         kind="project" [tag=] [role=] [status=]
tether_registry_update_self    urn= patch={...}
tether_registry_deregister     urn=
tether_registry_sync           urn=
tether_registry_merge          urn= into=

mux registry register          --kind project --file project.json
mux registry lookup            <urn> [--include external_ids] [--full] [--json]
mux registry lookup-by         --kind project --external-id <id> [--substrate <s>] [--include external_ids] [--full] [--json]
mux registry search            --kind project [--tag <tag>] [--status <status>] [--json]
mux registry update-self       --file patch.json
mux registry deregister        <urn>
mux registry sync              <urn>
mux registry merge             <src_urn> <dst_urn>
```

---

## Core Operations

### 1. Onboarding (Register)

When a project is onboarded, `agent-setup` creates its canonical registry profile.
The caller provides authored fields and known substrate IDs. Tether mints a unique
canonical URN (`msg://project/project-mux/prj_xxxxxxxxxx`).

#### Request (HTTP)
```http
POST /registry/projects HTTP/1.1
Content-Type: application/json

{
  "display_name": "Tether Control Plane",
  "description": "Local agent session control plane daemon and CLI",
  "guidelines": "Always verify with `make check` before closing tasks.",
  "tags": ["runtime", "control-plane", "go"],
  "entry_points": ["cmd/mux/main.go", "internal/app/service.go"],
  "props": {
    "docs_url": "https://tether.example.com",
    "project_root": "/Users/chrispian/dev/hollis-labs/apps/tether"
  },
  "external_ids": [
    {"substrate": "tether", "external_id": "tether"},
    {"substrate": "torque", "external_id": "PRJ-TETHER-01"},
    {"substrate": "cerberus", "external_id": "tether-runtime"}
  ],
  "callback": {
    "scheme": "cli",
    "target": "mux describe --json"
  }
}
```

#### Response
```http
HTTP/1.1 201 Created
Content-Type: application/json

{
  "urn": "msg://project/project-mux/prj_01m2gtwv16",
  "kind": "project",
  "display_name": "Tether Control Plane",
  "description": "Local agent session control plane daemon and CLI",
  "guidelines": "Always verify with `make check` before closing tasks.",
  "tags": ["runtime", "control-plane", "go"],
  "entry_points": ["cmd/mux/main.go", "internal/app/service.go"],
  "props": {
    "docs_url": "https://tether.example.com",
    "project_root": "/Users/chrispian/dev/hollis-labs/apps/tether"
  },
  "status": "active",
  "created_at": "2026-09-14T16:00:00Z",
  "updated_at": "2026-09-14T16:00:00Z"
}
```

*(Note: Sensitive operational fields and external IDs are omitted from default responses by the bidirectional redaction policy; see [Redaction & Scope Policy](#redaction--scope-policy).)*

---

### 2. Resolve (Given Project, Get All App IDs)

Callers use direct URN lookup with `include=external_ids` to resolve a project's IDs
across every registered substrate.

#### Request (HTTP)
```http
GET /registry/projects/msg%3A%2F%2Fproject%2Fproject-mux%2Fprj_01m2gtwv16?include=external_ids HTTP/1.1
```

#### Request (MCP)
```json
{
  "name": "tether_registry_lookup",
  "arguments": {
    "urn": "msg://project/project-mux/prj_01m2gtwv16",
    "include": "external_ids"
  }
}
```

#### Request (CLI)
```bash
mux registry lookup msg://project/project-mux/prj_01m2gtwv16 --include external_ids
```

#### Response
```json
{
  "urn": "msg://project/project-mux/prj_01m2gtwv16",
  "kind": "project",
  "display_name": "Tether Control Plane",
  "status": "active",
  "external_ids": [
    {
      "substrate": "tether",
      "external_id": "tether",
      "attached_at": "2026-09-14T16:00:00Z"
    },
    {
      "substrate": "torque",
      "external_id": "PRJ-TETHER-01",
      "attached_at": "2026-09-14T16:00:00Z"
    },
    {
      "substrate": "cerberus",
      "external_id": "tether-runtime",
      "attached_at": "2026-09-14T16:00:00Z"
    }
  ]
}
```

---

### 3. Reverse Lookup (Given App ID, Get Project)

When an agent or tool has a substrate-local identifier (such as a Torque project ID `PRJ-TETHER-01`
or a Tether catalog slug `tether`), `LookupBy` resolves it back to the canonical project URN and profile.

#### Request (HTTP)
```http
GET /registry/projects?external_id=PRJ-TETHER-01&substrate=torque&include=external_ids HTTP/1.1
```

#### Request (MCP)
```json
{
  "name": "tether_registry_lookup_by",
  "arguments": {
    "kind": "project",
    "external_id": "PRJ-TETHER-01",
    "substrate": "torque",
    "include": "external_ids"
  }
}
```

#### Request (CLI)
```bash
mux registry lookup-by --kind project --external-id PRJ-TETHER-01 --substrate torque --include external_ids
```

#### Response
```json
{
  "project": {
    "urn": "msg://project/project-mux/prj_01m2gtwv16",
    "kind": "project",
    "display_name": "Tether Control Plane",
    "status": "active",
    "external_ids": [
      {"substrate": "tether", "external_id": "tether", "attached_at": "2026-09-14T16:00:00Z"},
      {"substrate": "torque", "external_id": "PRJ-TETHER-01", "attached_at": "2026-09-14T16:00:00Z"},
      {"substrate": "cerberus", "external_id": "tether-runtime", "attached_at": "2026-09-14T16:00:00Z"}
    ]
  }
}
```

---

## Derived vs. Authored Field Split

Project profiles maintain a strict separation between authored metadata and derived state (C1, ADR 0041).

| Classification | Fields | Source & Mutation Rules |
|---|---|---|
| **Authored** | `props`, `description`, `guidelines`, `tags`, `entry_points`, `capabilities`, `skills`, `links`, `title`, `role`, `avatar` | Hand-authored during onboarding or updated via `UpdateSelf`. **Protected against Sync clobbering**: Sync never overwrites authored fields. `props` is a flat, open string-to-string map for arbitrary project facts (e.g. `docs_url`, `project_root`, `help_file`, `inbox`, `primary_agent`). |
| **Derived** | `display_name`, `project`, `status`, `health_status`, `host_address`, `last_seen_at`, `kind_meta` | Populated and refreshed automatically by callback resolvers during `Sync`. |

### Field Metadata & Provenance
Every field carries provenance tracking in `field_metadata`:
- `class`: `"authored"` or `"derived"`.
- `last_updated_by`: Identity string asserting the touch.
- `cached_at`: Timestamp indicating when the value was refreshed from an upstream callback.

---

## Sync Callbacks (C2)

A project may declare a callback URI that provides live status and metadata:

```json
"callback": {
  "scheme": "cli",
  "target": "mux describe --json"
}
```

### Supported Callback Schemes
- `file://`: Reads a sparse JSON/YAML profile from the filesystem.
- `cli://`: Executes a local CLI command returning JSON/YAML (requires CLI resolver configuration).

### Last-Good State Preservation
If a callback resolver fails (command exits non-zero, file missing, or invalid JSON):
- `POST /sync` fails immediately with `502 Bad Gateway`.
- The stored database row is **not** modified.
- Existing values (both derived and authored) remain completely intact at their last-known good values.

---

## Substrate Identifiers & The Torque Substrate

### Substrate Naming
Substrates are free-form strings (`"torque"`, `"tether"`, `"cerberus"`, `"github"`). No hardcoded allowlist or enum restricts substrate names.

### Attaching Substrate IDs
1. **At Onboarding**: Supply `external_ids` in the `Register` body.
2. **Post-Onboarding**: Register an entry for the new substrate key and call `Merge(newURN, targetURN)`. The destination absorbs the external ID.
3. **Storage Rule**: `(urn, substrate)` is unique. A project may have at most one external ID per substrate.

### Explicit Mapping, Not Heuristic Inference (`CW-20260914-0022`)
Tether intentionally does **not** scrape Torque or guess cross-substrate IDs by coincidental string matching. Mappings must be explicitly asserted by `agent-setup` onboarding or by operators.

---

## Partial-Coverage Discipline

> *"Absent must never imply forged."*

Not every project exists on every substrate:
- A project registered in Tether and Cerberus that has **no** Torque task project is **complete and valid**.
- Resolving external IDs returns an ordinary list containing only the registered substrates. Missing substrates are **omitted without error or missing-data sentinels**.
- Performing a reverse lookup (`LookupBy`) for an unattached substrate ID returns an ordinary `404 Not Found` / `not_found` error, never a server panic or schema error.

---

## Redaction & Scope Policy

Under `CW-20260912-0053`, `CW-20260912-0096`, `CW-20260914-0038`, and `CW-20260914-0039`:

### Visible vs. Gated Fields
- **Visible by Default**: All authored metadata — including the open `props` bag, `description`, `tags`, `guidelines`, and `entry_points` — is visible by default in all queries (Register, Lookup, Search). Projects publishing their own props want them seen without friction.
- **Correlation Identifiers**: `external_ids` (substrate mappings like `PRJ-001`). Omitted from default search/item responses to keep discovery lightweight, but accessible with standard read scope via `?include=external_ids` / `include="external_ids"`.
- **Sensitive Operational Fields**: `callback` (internal command/file path), `host_address` (internal IP/interface), `kind_meta` (native private daemon metadata). Omitted by default and strictly require `registry.write` scope.

### Access Rules Across Surfaces
1. **Default Queries**: Returns public profile with authored fields (including `props`), dropping `callback`, `host_address`, `kind_meta`, and `external_ids`.
2. **HTTP Surface**: Callers can selectively include fields via `?include=external_ids` (read scope) or `?full=true` / sensitive includes (write scope).
3. **CLI Surface**: `mux registry lookup` and `mux registry lookup-by` accept `--include <fields>` and `--full`.
4. **MCP Surface**:
   - Including `external_ids` is **accessible with read scope** (no elevated scope required).
   - Including `callback`, `host_address`, `kind_meta`, or `all` requires the `registry.write` scope.

---

## Offboarding & Terminal States

Offboarding does not delete rows; it records unambiguous terminal states:

### 1. Deregister (Deprecation)
- Invoked via `DELETE /registry/projects/{urn}`, MCP `tether_registry_deregister`, or `mux registry deregister`.
- Flips `status` to `"deprecated"`.
- Deprecated rows are excluded from default search, but **remain resolvable by direct URN lookup** for auditability.

### 2. Merge (Deduplication)
- Invoked via `POST /registry/projects/{urnSrc}/merge {"into": "{urnDst}"}`, MCP `tether_registry_merge`, or `mux registry merge`.
- Source profile transitions to `status: "merged"` and records `merged_into: "<dstURN>"`.
- Destination profile absorbs all external ID mappings and array fields from the source.
- Reverse lookups for any of the source's external IDs immediately resolve to the destination URN.
