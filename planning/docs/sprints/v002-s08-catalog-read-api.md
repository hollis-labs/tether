# Sprint v002-s08 — Catalog Read API (retro)

**Retro-added to v0.0.2** on 2026-04-19 to unblock v0.0.3 TUI MVP Sprint 1+.
**Scope:** read-only list endpoints for the four catalog types (projects / agents / providers / launches). Writes are explicitly out of scope — they belong to a future write-API sprint or to v0.0.3 Sprint 3 unparking discussions.
**Epic:** n/a (retro-sprint against shipped v0.0.2 Runtime Foundation).
**Transport/shape inheritance:** matches existing v0.0.2 local API conventions (UDS default, `daemon.listen_addr` respected, typed error envelope, UTF-8 JSON).

**Consumers waiting on this:**
- `agent-mux/internal/tui/client/` (v003-s01-03) — `ListProjects` / `ListAgents` / `ListProviders` / `ListLaunches`.
- TUI results viewport population (v003-s01-04).
- Sprint 5 "runtime concepts surface" (indirectly, via the same list types).

**Parallelism note:** This sprint runs in a separate agent-mux session in parallel with v003-s01. Use a worktree — `git worktree add ~/Projects-apps/agent-mux-v002-s08 feat/v002-s08-catalog-read-api` from the primary repo. Merges sequence-into-main: land v002-s08 first if ready, then v003-s01. If v003-s01 lands first, reconcile the TUI client in a follow-up pass.

---

## Exit criteria

- [x] `GET /catalog/projects` returns the list of project definitions loaded from the catalog root.
- [x] `GET /catalog/agents` returns the list of agent definitions.
- [x] `GET /catalog/providers` returns the list of provider definitions.
- [x] `GET /catalog/launches` returns the list of launch profiles.
- [x] All four endpoints emit `200 OK` with a typed JSON response; malformed-catalog surfaces as `500 internal_error` with a helpful message (not a panic).
- [x] Response DTOs live in `internal/api/catalog_types.go` and are reused by the TUI client package once wired.
- [x] Handler implementation lives in `internal/api/catalog.go`, registered from `internal/api/server.go` alongside the existing session/broker/checkpoint registrations.
- [x] `internal/client/` gains typed methods: `ListProjects(ctx) ([]config.Project, error)` and siblings — mirroring existing session-list helpers — so both the CLI and TUI-client share the same surface.
- [x] Handler tests (`internal/api/catalog_test.go`) exercise each endpoint against a fixture catalog root.
- [x] Client tests (`internal/client/client_test.go`) exercise each method against `httptest.Server`.
- [x] `docs/api/README.md` gains a `## Catalog` section documenting the four routes, request/response shapes, and the read-only scope note.
- [x] ADR `docs/adr/0012-catalog-read-api.md` captures the decision: why reads land now, why writes are deferred, and the trust model (same-host UDS, no auth).
- [x] `make check` green (`fmt + vet + lint + test-race + vuln`).

---

## Decisions locked (don't reopen in this sprint)

- **D1 Reads only.** Writes (create / update / delete / reload) are explicitly deferred. Don't stub POST/PUT/DELETE handlers with `501 not_implemented` either — leave the routes undefined so the error envelope path surfaces `method_not_allowed` naturally if a client probes them, and so future-you isn't tempted to "almost ship" writes.
- **D2 URL shape: `/catalog/<type>`.** Keeps catalog things grouped; symmetric with future `POST /catalog/<type>`; does not collide with runtime `/sessions` or `/broker/*`.
- **D3 Response envelope matches existing list endpoints.** `ListSessions` returns `{"sessions": [...], "next_cursor": "..."}`. For the catalog, drop `next_cursor` (catalog sizes are small — <~100s per type) and use `{"projects": [...]}` etc.
- **D4 DTOs = pass-through of `config.Project`/`Agent`/`Provider`/`Launch`.** Don't design a separate API-layer DTO unless `config.*` has a field the API should hide (e.g., absolute paths). If it does, hide it with json tags or a small DTO step; otherwise, ship `config.*` directly.
- **D5 No filtering / pagination / search params** in this sprint. Clients load the full set and filter locally. Matches the TUI's client-side fuzzy design.
- **D6 Catalog resolution matches existing loader.** The API reuses whatever `config.Loader` / `config.Paths` helpers the CLI uses. Don't reimplement catalog discovery inside the handler.
- **D7 Error model: no new error codes.** Use the seven existing codes from ADR 0010. Malformed catalog → `internal_error`; no such catalog root → `internal_error` with an actionable message (mention the expected path, e.g. `~/.agent-mux/catalog/`).

---

## Tasks

### T-v002-s08-01: Catalog list handlers + DTOs + registration

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, api, catalog, readonly]

#### Problem

The daemon has no way to serve the catalog today. The CLI reads catalog files directly off disk; no HTTP surface exposes them. The v0.0.3 TUI needs a typed remote read path.

#### Fix direction

- New file `internal/api/catalog.go` with:
  - `registerCatalog(mux *http.ServeMux, s *Server)` called from `server.go` alongside the existing `registerSessions` / `registerBroker` / etc.
  - Four handlers: `handleListProjects`, `handleListAgents`, `handleListProviders`, `handleListLaunches`.
  - Each handler: resolves the catalog root (reuse existing helper in `internal/config/paths.go`), loads via the existing loader, returns `{"<type>": [...]}`.
  - GET-only. Non-GET → `method_not_allowed` via the existing error envelope helper.
- Response DTOs in `internal/api/types.go` (or a new `catalog_types.go`): one response struct per type. If `config.*` structs are already JSON-annotated and contain no sensitive fields, embed them directly.
- Wire `registerCatalog` from `internal/api/server.go`.
- Handle the "catalog root missing" case: return `internal_error` with a message like `"catalog root not found at <path>: run mux bootstrap or set AGENT_MUX_CATALOG"`. Don't panic; don't 404 (the route exists).
- Handle the "catalog present but malformed" case the same way but with a message citing the offending file.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/api/catalog.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/types.go` (extend, or new `catalog_types.go`)
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/server.go` (register)
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/catalog_test.go` (new)

#### Acceptance criteria

- [x] GET each of the four routes against a fixture catalog returns the expected typed list.
- [x] Non-GET on the routes returns `405 method_not_allowed`.
- [x] Missing catalog root returns `500 internal_error` with an actionable message.
- [x] Malformed catalog (e.g., invalid YAML in one file) returns `500 internal_error` naming the offending path.
- [x] Handler tests cover happy path + both error paths per route.

#### Test plan

- Unit: table-driven `httptest.Server` tests per route using a temp-dir catalog fixture.
- Unit: error-path tests construct a bad fixture (missing dir, malformed YAML) and assert the error envelope.
- Race: `go test -race ./internal/api/...` clean.

#### Scope fences

- No POST/PUT/PATCH/DELETE handlers. If one appears in a diff, delete it.
- No `/catalog` index/root endpoint enumerating types — the TUI doesn't need it.
- No filtering, sorting, or pagination query params.
- No event emission on reads. Reads are pure.

---

### T-v002-s08-02: Client methods + CLI convenience

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, client, catalog]

#### Problem

With handlers live, `internal/client/` needs typed methods so the TUI client (landing in v003-s01-03) and any future CLI subcommand can call them without re-implementing transport.

#### Fix direction

- Extend `internal/client/client.go`:
  - `ListProjects(ctx) ([]config.Project, error)`
  - `ListAgents(ctx) ([]config.Agent, error)`
  - `ListProviders(ctx) ([]config.Provider, error)`
  - `ListLaunches(ctx) ([]config.Launch, error)`
- Each method uses the existing `getJSON` helper; wraps unreachable errors with `ErrDaemonUnreachable`.
- No CLI subcommand additions required for this sprint — but if `cmd/mux/projects.go` currently reads the filesystem directly, leave a `// TODO v002-s08: route through daemon` comment where the switch would eventually happen. Don't actually switch the CLI over in this sprint (out of scope; CLI reshuffle is a separate concern).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/client/client.go` (extend)
- `/Users/chrispian/Projects-apps/agent-mux/internal/client/client_test.go` (extend)
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/projects.go` / `agents.go` — single-line TODO only, if applicable

#### Acceptance criteria

- [x] Four new client methods ship with tests against `httptest.Server`.
- [x] `ErrDaemonUnreachable` path covered per method.
- [x] No CLI subcommand behavior change.

#### Test plan

- Unit: `httptest.Server` canned-response tests per method.
- Unit: server-500 and server-unreachable paths per method.

#### Scope fences

- Don't refactor existing client helpers.
- Don't add a `Client.ListCatalog()` meta-method that fans out to all four — keep the four typed methods explicit.
- Don't add retry/backoff. Callers do that.

---

### T-v002-s08-03: API docs + ADR 0012

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [docs, adr, catalog]

#### Fix direction

- `docs/api/README.md`: add a `## Catalog` section after `## Events`, documenting the four endpoints, response shape, read-only scope note, and a pointer to ADR 0012 for the "why reads first, why not writes" rationale.
- `docs/adr/0012-catalog-read-api.md`: ADR capturing the decision. Next free ADR number per `boot-prompt.md` is 0012.
  - **Context:** v0.0.3 TUI needs a remote catalog read path; writes are not required for MVP; same-host UDS trust model permits reads without auth.
  - **Decision:** ship `/catalog/<type>` GET endpoints now; defer writes.
  - **Consequences:** TUI does not need to re-implement catalog loading; CLI can optionally route through daemon later; write path needs its own ADR (validation model, concurrent-writer policy, hot-reload, optimistic locking) when it lands.
  - **Alternatives considered:** TUI reads filesystem directly (rejected — split brain between CLI and TUI paths); combined read+write API in one sprint (rejected — write design is a separate discussion worth doing without schedule pressure).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/docs/api/README.md` (extend)
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0012-catalog-read-api.md` (new)

#### Acceptance criteria

- [x] API doc section exists with example requests/responses for all four endpoints.
- [x] ADR 0012 is written, dated, sequential, and linked from the API doc.

---

## Review / readiness notes

- **Catalog root resolution.** `internal/config/paths.go` already resolves `AGENT_MUX_CATALOG` → `~/.agent-mux/catalog/` with fallbacks. Reuse it — don't reimplement. If the helper doesn't exist there, extract it before adding handlers.
- **Loader error shape.** If `config.Loader` returns a loud multi-error for a malformed catalog, decide during T-01 how to summarize it in the error envelope — probably take the first failure + a count. Don't dump stack traces.
- **Trust model.** UDS filesystem permissions gate access. Don't add auth; don't add rate limits; don't log request bodies. Mirrors the rest of the v0.0.2 API.
- **Out-of-band note for CLI reconciliation.** `mux projects list` etc. currently read the filesystem directly. Moving them behind the daemon is a separate decision (affects daemon-down UX); don't do it in this sprint.
- **Out-of-band note for live catalog updates.** When another session mutates the catalog (e.g., the user edits a YAML file), the TUI's cached list goes stale. Invalidation is NOT in scope here — the TUI adds a manual refresh key in a later sprint, and event-driven invalidation arrives when write endpoints ship.

---

## Done checklist (at sprint close)

- [x] All three task Acceptance sections ticked.
- [x] Exit criteria above all ticked.
- [x] `make check` green.
- [x] ADR 0012 committed.
- [x] Branch FF-merged to `main`, branch deleted.
- [ ] Consumer session (v003-s01-03) notified so the TUI client can wire the new methods.

---

## Hand-off snippet for the parallel agent

> You're executing a retro-sprint (v0.0.2 Sprint 8) that unblocks the v0.0.3 TUI MVP. Your scope is four read-only HTTP endpoints on the muxd daemon: `GET /catalog/{projects,agents,providers,launches}`. Writes are out of scope. Read `boot-prompt.md` for project state, this sprint file for the plan, ADR 0011 for the existing local-API error model, and `internal/api/server.go` for the handler registration pattern. Use a worktree at `~/Projects-apps/agent-mux-v002-s08/` on branch `feat/v002-s08-catalog-read-api`. FF-merge at close.
