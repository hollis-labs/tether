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
POST   /registry/reonboard                              # Reonboard legacy project rows

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
mux registry reonboard
```

---

## Core Operations

### 1. Composable Onboarding Sequence

The onboarding contract is designed as a sequence of composable, independent steps rather than one monolithic call.
Tether provides the contract, persistence plumbing, and HTTP/MCP/Go client surface; `agent-setup` owns the
orchestration workflow that guides human or agent decisions (per `CW-20260912-0096` and `CW-20260914-0041`).

#### Step 1: Mint Canonical Identity (MVP Core Path)
The bare-minimum entry point: register the project with its display name, optional description, optional owner,
and optional derivation pointer/callback. This mints a canonical URN (`msg://project/project-mux/prj_...`) in `status: active`.
Everything else can be layered on incrementally.

##### Request (HTTP)
```http
POST /registry/projects HTTP/1.1
Content-Type: application/json

{
  "display_name": "Tether Control Plane",
  "description": "Local agent session control plane daemon and CLI",
  "callback": {
    "scheme": "cli",
    "target": "mux describe --json"
  }
}
```

##### Request (Go Client)
```go
profile, err := client.Registry().OnboardProject(ctx, client.OnboardProjectParams{
    DisplayName: "Tether Control Plane",
    Description: "Local agent session control plane daemon and CLI",
    Callback: &registry.Callback{
        Scheme: "cli",
        Target: "mux describe --json",
    },
})
```

##### Response
```http
HTTP/1.1 201 Created
Content-Type: application/json

{
  "urn": "msg://project/project-mux/prj_01m2gtwv16",
  "kind": "project",
  "display_name": "Tether Control Plane",
  "description": "Local agent session control plane daemon and CLI",
  "status": "active",
  "created_at": "2026-09-14T16:00:00Z",
  "updated_at": "2026-09-14T16:00:00Z"
}
```

#### Step 2: Attach Authored Metadata & Props
Layer on project facts using the open, flat `props` bag (e.g. `docs_url`, `project_root`, `primary_agent`),
guidelines, tags, or entry points via `PATCH /registry/projects/{urn}` (`UpdateSelf`).
Authored fields are protected against sync clobbering.

```http
PATCH /registry/projects/msg%3A%2F%2Fproject%2Fproject-mux%2Fprj_01m2gtwv16 HTTP/1.1
Content-Type: application/json

{
  "props": {
    "docs_url": "https://tether.example.com",
    "project_root": "/Users/chrispian/dev/hollis-labs/apps/tether"
  },
  "tags": ["runtime", "control-plane", "go"],
  "guidelines": "Always verify with `make check` before closing tasks."
}
```

#### Step 3: Attach Substrate External Identifiers
Link external substrate IDs (e.g. Torque `PRJ-xxxx`, Cerberus site name, GitHub repo).
Substrate IDs can be supplied either during initial registration or subsequently via deduplicating `Merge`:

```http
POST /registry/projects/msg%3A%2F%2Fproject%2Fproject-mux%2Fprj_tmp/merge HTTP/1.1
Content-Type: application/json

{
  "into": "msg://project/project-mux/prj_01m2gtwv16"
}
```
Enforces 1:1 mapping per substrate (`(urn, substrate)` uniqueness) and enables reverse lookup (`LookupBy`).

#### Step 4: Attach Tesseract Knowledge Namespace
Record the project's Tesseract namespace (e.g. `user/chrispian/knowledge/tether`) via `props["tesseract_namespace"]`
or `links`. Tether sessions and CLI agents use this to recall architectural decisions, investigations, and skills:

```http
PATCH /registry/projects/msg%3A%2F%2Fproject%2Fproject-mux%2Fprj_01m2gtwv16 HTTP/1.1
Content-Type: application/json

{
  "props": {
    "tesseract_namespace": "user/chrispian/knowledge/tether"
  }
}
```

#### Step 5: Record Tooling & Auth Preferences (Placeholder / Opt-in)
Record project-level tool opt-ins or policy preferences (e.g. enabled MCP servers, allowed LLM models).
The actual authorization boundary is enforced at session launch; this step records the project preference
declaratively in `props` without requiring immediate auth implementation:

```http
PATCH /registry/projects/msg%3A%2F%2Fproject%2Fproject-mux%2Fprj_01m2gtwv16 HTTP/1.1
Content-Type: application/json

{
  "props": {
    "mcp_opt_in": "torque,mux,tesseract",
    "llm_policy": "claude-3-5-sonnet"
  }
}
```

*(Note: Sensitive operational fields and external IDs are omitted from default responses by the bidirectional redaction policy; see [Redaction & Scope Policy](#redaction--scope-policy).)*

---

### Onboarding Settings Cascade (Global > Project > User)

Not every deployment, project, or user shares the same onboarding expectations (such as required props,
whether MCP opt-in is offered, or default LLM policies). Tether provides a dedicated settings store in `state.db`
and a closest-wins resolution cascade (`internal/settings`, `CW-20260914-0042`):

- **User tier** (`scope: "user"`, `scope_id: "<user_urn>"`): Closest to the actor; overrides project and global settings where specified.
- **Project tier** (`scope: "project"`, `scope_id: "<project_urn>"`): Local to the project; overrides deployment global defaults.
- **Global tier** (`scope: "global"`, `scope_id: ""`): Fleet-wide default configuration.
- **Code-level fallback**: Defaults applied when no tier specifies a value.

#### Resolution Endpoint
```http
GET /settings/onboarding?project=msg%3A%2F%2Fproject%2Fproject-mux%2Fprj_01&user=msg%3A%2F%2Fagent%2Fagent-mux%2Fusr_01 HTTP/1.1
```
##### Response
```json
{
  "required_props": ["docs_url", "project_root"],
  "mcp_opt_in_offered": true,
  "default_llm_policy": "claude-3-5-sonnet",
  "custom": {
    "env": "production"
  }
}
```

#### Scoped CRUD Endpoints
- `GET /settings/onboarding/{scope}?scope_id={id}`
- `PUT /settings/onboarding/{scope}?scope_id={id}`

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

### 4. Re-onboarding Existing Project Rows (CW-20260914-0044)

Existing project rows originally seeded by passive catalog bootstrap importers are explicitly migrated to the modern contract via:
- **HTTP**: `POST /registry/reonboard`
- **Go Client**: `client.Registry().Reonboard(ctx)`
- **CLI**: `mux registry reonboard`

The re-onboarding operation:
1. Scans catalog project definitions (if configured) and updates or registers project profiles under the new contract.
2. Sweeps all existing database project rows carrying legacy bootstrap status (`LastUpdatedBy == "system:bootstrap"` or `"system:merge"`), unpopulated props bags, or legacy `kind_meta` data.
3. Migrates `repo_root` and `tracking_root` from `kind_meta` into open, authored `props`.
4. Populates `tesseract_namespace` under `props["tesseract_namespace"]` (`user/chrispian/knowledge/<slug>`).
5. Attaches `substrate="tether"` external IDs.
6. Sets `LastUpdatedBy = "operator:re-onboard"`.
7. Preserves authored provenance in `field_metadata` asserting `FieldClassAuthored` for `props`.

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

### Explicit Mapping, Not Heuristic Inference (`CW-20260914-0022`, `CW-20260914-0043`)
Tether intentionally does **not** scrape Torque or guess cross-substrate IDs by coincidental string matching. With the retirement of passive startup bootstrap importers (`CW-20260914-0043`), accidental cross-app string matching and duplicate generation are completely eliminated. Mappings must be explicitly asserted by `agent-setup` onboarding or by operators.

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
