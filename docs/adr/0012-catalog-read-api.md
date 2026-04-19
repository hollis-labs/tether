# ADR 0012: Local API — Catalog Read Endpoints (Reads Now, Writes Deferred)

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-s08 (Catalog Read API, retro-added to v0.0.2 to unblock v0.0.3 TUI MVP)
**Deciders:** agent-mux v0.0.2 execution session

## Context

The v0.0.2 daemon exposes HTTP surfaces for session lifecycle, attach
streaming, checkpoints, broker envelopes, and event observation, but
nothing for the *catalog itself* — the set of projects, agents,
providers, and launch profiles that drive session creation. The CLI
reads catalog YAML files directly off disk via `app.New(catalogPath)`;
no HTTP route exposes those shapes.

The v0.0.3 TUI MVP (Sprint v003-01 T-03/T-04) is daemon-only by epic
decision D7, and its main-screen results viewport needs to list
catalog objects. Without a remote read path, the TUI would have to
either (a) re-implement catalog discovery client-side — forking the
CLI's read path and producing two implementations that can drift — or
(b) fall back to an in-process composition that contradicts D7.

Writes (create/update/delete/reload) are not required for the TUI
MVP's first shippable surface (Sprint 1) and their design raises
open questions — validation model, concurrent-writer policy, hot-
reload semantics, optimistic locking — that are worth their own
dedicated sprint rather than being smuggled in alongside reads.

## Decision

**Ship four read-only list endpoints under `/catalog/<type>` now.
Defer all write paths to a future sprint with its own ADR (0013).**

- `GET /catalog/projects` → `{"projects": [Project, …]}`
- `GET /catalog/agents` → `{"agents": [Agent, …]}`
- `GET /catalog/providers` → `{"providers": [Provider, …]}`
- `GET /catalog/launches` → `{"launches": [Launch, …]}`

Response shape rules:

- **Envelope per list endpoint** — matches the `ListSessions` pattern
  (ADR 0009 conventions), keyed by the plural type name.
- **No pagination / no cursor** — catalog sizes are small (O(100s) per
  type). Clients load full sets and filter locally. This matches the
  TUI's client-side fuzzy-search design.
- **No filtering / sorting query params** — kept minimal for MVP.
  Results are sorted by ID server-side so TUI clients don't re-render
  churn between refreshes.
- **DTOs = direct `config.*` structs** with `json:` tags added for
  wire-contract hygiene. No separate API-layer DTO layer — catalog
  records contain no sensitive fields that need hiding.

Transport + error model rules:

- **Reuses v0.0.2 conventions** — UDS default, `daemon.listen_addr`
  respected, typed error envelope (ADR 0010), UTF-8 JSON.
- **No new error codes** — malformed catalog or missing root → the
  existing `internal_error` with a message that cites the offending
  path.
- **Fresh reads per request** — the handler calls a `CatalogLoader`
  that invokes `config.Load(catalogRoot)` on each call. Live YAML
  edits are picked up without a daemon restart. Read cost is trivial
  at catalog scale. Tests pass in a fake loader.
- **No `Validate()` call in the handler.** Dangling references (a
  launch pointing at a deleted project) still render; the TUI can
  show a visual warning. Validation remains a daemon-startup gate.

Write deferral:

- No POST / PUT / PATCH / DELETE handlers exist at the route path.
  Non-GET requests hit the existing `method_not_allowed` envelope path.
- No `501 not_implemented` stubs for writes — leaving the write routes
  truly undefined avoids priming "almost-done" energy for a design
  that isn't settled.

## Alternatives Considered

- **TUI reads the catalog filesystem directly.** Rejected: produces
  two catalog-reading implementations (CLI and TUI), which will drift.
  Also violates epic D7 (daemon-only); the TUI would need to know the
  same `~/.agent-mux/catalog/` conventions as the daemon.
- **Combined read + write API in a single sprint.** Rejected: write
  design is a separate discussion (validation, concurrent writers,
  hot-reload, optimistic locking) whose scope and pace shouldn't be
  pinned to TUI unblock pressure. Shipping reads unlocks the TUI
  without committing to write semantics.
- **Return `501 not_implemented` for POST/PUT/DELETE on
  `/catalog/<type>`.** Rejected: invites "we almost shipped writes"
  framing and makes the route namespace look richer than it is.
  Undefined routes naturally route to `method_not_allowed` via the
  error envelope helpers.
- **Cache the catalog at daemon startup (mirror `app.Service.Catalog`
  behavior).** Rejected for this ADR: fresh reads honor the "malformed
  catalog cites the offending file" error story from the sprint and
  make editor workflows Just Work. Cost is trivial. A future write
  sprint can introduce explicit invalidation if cost ever becomes real.
- **Add `?limit` / `?cursor` pagination.** Rejected: catalog sizes are
  O(100s) per type — pagination is architectural overhead for a
  problem that doesn't exist. If a catalog grows past ~1000 entries
  of any type, we'll revisit.
- **Expose a `/catalog` index enumerating types.** Rejected: the TUI
  already knows the four types; discovery would be premature.

## Consequences

- TUI Sprint 1 T-03/T-04 unblocked; `internal/client/client.go` ships
  four typed methods (`ListProjects/Agents/Providers/Launches`) that
  the TUI can consume.
- CLI `mux projects list` / `mux agents list` continue reading the
  filesystem directly (no behavior change in this sprint). A TODO
  marker notes the future daemon-routing opportunity; the actual
  switch is a separate CLI-reshuffle decision (affects daemon-down
  UX).
- Trust model is same-host UDS-filesystem-permissions, per the rest
  of the v0.0.2 API. No auth on reads; no payload logging; no rate
  limits.
- Writes are a known future sprint. When they land, ADR 0013 will
  cover: validation model (how loud to be on semantic errors),
  concurrent-writer policy, hot-reload coordination with running
  sessions, optimistic locking, and event emission (`catalog.updated`
  kinds).
- Live YAML edits show up on the next API call without a daemon
  restart. The TUI's planned manual refresh key remains worthwhile
  for dedup / debounce reasons, not for freshness reasons.

Related ADRs:
[0009](0009-local-api-create-launch-split.md),
[0010](0010-local-api-typed-error-envelope.md),
[0011](0011-local-api-attach-transport.md).

Full endpoint reference: [`docs/api/README.md`](../api/README.md#catalog).
