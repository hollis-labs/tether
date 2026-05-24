# ADR 0043: Cross-Substrate Registry Dedup

- Status: Accepted
- Date: 2026-05-24
- Deciders: Tether maintainers
- Supersedes: none
- Related: [ADR 0041](0041-registry-directory-service.md)

## Context

Tether's registry became the portfolio-wide public identity surface in ADR 0041,
but each substrate still owns its own local catalog. Tether's first-party
catalog and Cerberus's local registry both describe overlapping projects. If
both bootstraps register those projects independently, the registry ends up
with duplicate rows for the same logical entity and discovery quality degrades
immediately.

We need a dedup primitive that:

- preserves opaque, server-minted URNs
- does not require globally coordinated slugs across substrates
- lets future substrates attach their own local identifiers to an existing URN
- keeps operational config in the owning substrate rather than in the registry

## Decision

Tether stores substrate-local identifiers in a sibling table:
`registry_external_ids(urn, substrate, external_id, attached_at)`.

The dedup primitive is:

- `LookupBy(kind, external_id, substrate?)`

Bootstrap importers use that primitive before `Register`:

- if `(kind, substrate, external_id)` already exists, the row already exists
- if the same `external_id` exists for the same `kind` under another substrate,
  attach the new substrate's external ID to that existing URN
- otherwise, register a new row and attach the substrate's external ID

The source YAML remains the operational source of truth. Tether writes
`registry_urn: <urn>` back into supported source files so the file can carry
its canonical shared-directory identity across path moves and index rebuilds.

## Consequences

- URNs remain opaque and stable even when a substrate renames its local slug.
- Substrates can federate onto an existing logical entity without re-registering
  it under a new URN.
- Callers can resolve by either canonical URN or `(substrate, external_id)`.
- Duplicate rows created before this dedup path existed require an explicit
  operator merge path; Tether provides that as `mux registry merge`.

## Alternatives considered

### Deterministic URNs from slugs

Rejected. That would break the opaque Stripe-style URN contract adopted in ADR
0041 and would couple registry identity to substrate-local naming.

### Require global slug uniqueness

Rejected. Substrates cannot be expected to coordinate one shared namespace for
their local IDs.

### Accept duplicates and clean them up later

Rejected. That would make the registry a noisy discovery surface from day one,
which defeats the purpose of a shared directory.
