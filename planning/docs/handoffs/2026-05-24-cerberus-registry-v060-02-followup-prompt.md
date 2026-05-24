# Cerberus Registry v060-02 Follow-Up Prompt

## Context

Cerberus reviewed its side of the project-registry migration on 2026-05-24 so
Tether can pick up the shared work once its current tasks finish.

## Key Findings

- Cerberus already has its own local-first project registry substrate in-tree:
  - app-owned project configs: `internal/registry/projectconfig.go`
  - pointer index at `~/.cerberus/registry.yaml`: `internal/registry/index.go`
  - registry-aware runtime resolution: `internal/registry/resolve.go`
- Cerberus is aligned to the shared standard in shape and behavior, but it is
  not using Tether's registry implementation. It remains a Cerberus-owned
  substrate.
- Tether has the v060-01 registry foundation shipped, but the v060-02
  cross-substrate dedup/bootstrap work does not appear landed yet:
  - migrations currently stop at `0016_group_messaging.sql`
  - `internal/registry/bootstrap.go` still says cross-substrate dedup is a
    future v060-02 concern
  - `internal/registry/storage.go` still says `LookupBy(external_id,
    substrate)` lands in v060-02
  - the plan in
    `planning/docs/sprints/v060-02-cross-substrate-bootstrap-dedup.md`
    remains the source of truth for the intended work

## Critical Compatibility Note

Tether's v060-02 plan says bootstrap should write `registry_urn: <urn>` back
into Cerberus `*.cerberus.yaml` files.

Cerberus currently rejects unknown top-level fields when loading project
configs:

- `internal/registry/projectconfig.go` uses `yaml.Decoder.KnownFields(true)`
- `internal/registry/schema.go` validates the current envelope but does not yet
  model `registry_urn`

That means Tether MUST NOT write `registry_urn` into Cerberus-owned config
files until Cerberus lands support for that field, or both sides ship in
lockstep.

## What Tether Should Do Next

Please execute the v060-02 cross-substrate work with Cerberus compatibility in
mind.

1. Land `0017_registry_external_ids.sql` and the storage/service seams for:
   - `LookupBy(kind, external_id, substrate?)`
   - `AttachExternalID`
   - Tether external-id backfill
2. Land the Cerberus bootstrap importer that reads `~/.cerberus/registry.yaml`
   as the source-of-truth index, not a directory walk.
3. Keep importer scope identity-only:
   - import `owner` as Cerberus external ID
   - import `project.name` as display name
   - do not copy `resources`, `config`, `env`, or secrets
4. Coordinate the `registry_urn` write-back with Cerberus:
   - either gate Cerberus write-back behind an opt-in that stays off until
     Cerberus support lands
   - or ship only after Cerberus accepts `registry_urn`
5. Expose the dedup lookup surface at parity across:
   - HTTP
   - MCP
   - CLI
6. Keep the row-count audit and merge-cleanup path from the sprint file as part
   of done criteria.

## Pointers

- Cerberus local registry package:
  `~/dev/hollis-labs/apps/cerberus/internal/registry/`
- Cerberus web/backend roadmap created from this audit:
  `~/dev/hollis-labs/apps/cerberus/docs/plans/gui-roadmap.md`
- Tether v060-02 sprint plan:
  `planning/docs/sprints/v060-02-cross-substrate-bootstrap-dedup.md`
- Tether registry overview:
  `docs/registry/overview.md`

## Suggested Execution Prompt

You are picking up the Cerberus follow-up for Tether's v060-02 cross-substrate
registry sprint.

Read these first:

- `planning/docs/sprints/v060-02-cross-substrate-bootstrap-dedup.md`
- `internal/registry/bootstrap.go`
- `internal/registry/storage.go`
- `docs/registry/overview.md`
- `~/dev/hollis-labs/apps/cerberus/internal/registry/projectconfig.go`
- `~/dev/hollis-labs/apps/cerberus/internal/registry/index.go`
- `~/dev/hollis-labs/apps/cerberus/internal/registry/resolve.go`

Then implement the missing v060-02 work. Treat this compatibility rule as
non-negotiable: do not write `registry_urn` into Cerberus `*.cerberus.yaml`
files until Cerberus has explicitly landed support for that field.
