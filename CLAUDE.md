# TETHER — Local Agent Session Control Plane

Per-user daemon (`muxd`) + CLI (`mux`) + MCP/ACP surfaces that own session
lifecycle, brokered messaging, and event streams for every CLI-backed agent in
the hollis-labs portfolio. Read `AGENTS.md` for the full project orientation;
this file is the harness-facing summary.

## Build & test

```bash
# Full gate (fmt + vet + lint + test-race + vuln + coverage-report)
make check

# Faster iteration
make test          # straight `go test`
make test-race     # race-enabled
make fmt vet lint  # static checks individually

# Build the daemon + CLI
go build ./cmd/mux/
```

**`make check` is the non-negotiable gate** at sprint close (and CI). Run it
before sending any ship notice or transitioning a Torque task to review.

## Architecture

- **`cmd/mux/`** — single CLI entry point. Cobra command tree:
  `daemon`, `sessions`, `agents`, `projects`, `launch`, `workspaces`,
  `mcp`, `acp`, `boot`/`boot-exec`/`generate-boot`, `resolve`, `registry`
  (post v060-01). `main.go` runs `rootCmd`; `root.go` wires the command list.
- **`internal/app/`** — composition root (`Service`). Start here when tracing
  how pieces wire together.
- **`internal/api/`** — HTTP/UDS daemon API. Typed error envelope per ADR
  0010. New endpoints (e.g. `/registry/*` from v060-01) extend this.
- **`internal/registry/` — TWO MEANINGS** (resolved at v060-01 Task 1):
  - The pre-v060-01 `internal/registry/` was the **launch-resolution layer**
    over `~/.tether/catalog/*` (read-only YAML walk for the launch path).
  - The post-v060-01 `internal/registry/` is the **federation directory
    service** (Register/Lookup/Search/UpdateSelf/Deregister/Sync). One of
    them is renamed in v060-01 Task 1; check `doc.go` for the current shape.
- **`internal/mcpadapter/`** — MCP stdio adapter + proxy aggregation. New
  registry MCP tools (`tether_registry_*`) land here per v060-01 Task 6.
- **`internal/store/`** — SQLite migrator + storage primitives.
  `internal/store/migrations/` holds the numbered SQL files.
- **`internal/client/`** — typed Go client for the HTTP API.
- **`docs/adr/`** — architecture decision records 0000-0039+. **Read recent
  ADRs before changing transport, provider, sandbox, MCP, or registry
  behavior.** ADR 0036 (hardening-standards) is the template for new ADRs.

## State roots

- **Canonical state root:** `~/.tether/`
- **Daemon socket:** `~/.tether/run/muxd.sock` (UDS, default transport)
- **State DB:** `~/.tether/state/tether.db` (migrations live here; registry
  added in v060-01 via migration `0015_registry.sql`-ish — verify actual
  number at sprint start)
- **Catalog YAMLs:** `~/.tether/catalog/{agents,projects,providers,launches,boot,mcp-servers,sandbox-profiles}/*.yaml`
- **Backward-compat symlink:** `~/dev/hollis-labs/apps/agent-mux` → `tether/`.
  Some in-repo docs (notably `README.md` and parts of `planning/`) still use
  the old name — canonical is `tether`.

## ADR conventions

- Numbered sequentially under `docs/adr/`.
- Format: `<NNNN>-<kebab-slug>.md`.
- Voice: terse, decision-oriented; alternatives considered + rationale.
- See `docs/adr/0036-hardening-standards.md` for a recent template.
- **When planning docs name an ADR number that's already taken** (e.g.
  v060-01's sprint doc references "ADR 0013" / "0014" which are existing
  Sandboxing / PTY-resize records), pick the next free number and update the
  sprint doc to match.

## Mux URN table (cross-substrate addresses)

URNs are message addresses; the directory service (v0.6) holds the identity
that each URN points at. Same-host UDS trust; no auth v1.

| URN | Role |
|---|---|
| `msg://agent/agent-mux/tether-registry-design` | Sprint authority for v0.6 federation directory work. Ship-notice sender; cross-substrate coordination point with cerberus + agridd. |
| `msg://agent/agent-mux/cerberus-registry-design` | Cerberus-side architect / coordination point for v0.6. |
| `msg://agent/agent-mux/agridd-keeper` | agridd-side keeper. Receives Sprint v060-01 ship notice (it gates agridd's Phase 3 Stage 3.d.2). Receives substrate findings + cross-substrate questions. |
| `msg://agent/agent-mux/tether-sprint-1-implementer` | Sprint v060-01 implementer's own URN (status reports + cross-substrate questions). Created by the .nanite/ config. |
| `msg://agent/agent-mux/tether-sprint-2-implementer` | Sprint v060-02 implementer's own URN. |

When the v060-01 ship notice goes out, it's sent **from**
`tether-registry-design` (not from the implementer's own URN) so the receiver's
expected-sender check matches. The architect URN remains the sprint authority
of record across sprints.

## Active sprints

- **v060-01 — Registry Foundation** — `planning/docs/sprints/v060-01-registry-foundation.md`. 9 tasks, 18 locked decisions, full directory-service surface for `agent` + `project` kinds. Gates agridd Phase 3 Stage 3.d.2. **First implementer**.
- **v060-02 — Cross-Substrate Bootstrap + Dedup** — `planning/docs/sprints/v060-02-cross-substrate-bootstrap-dedup.md`. 8 tasks. **Must not start until v060-01 ships.**

Sprint files are the spec; each task has acceptance + files + scope fences.
Torque tasks are optional supplementary tracking (sprint-file checkboxes are
the primary tracker for this epic).

## Nanite agents in this repo

Agents bootable in a Claude Code session from this repo:

- `tether-sprint-1-implementer` — boot context `.nanite/agents/tether-sprint-1-implementer.md`
- `tether-sprint-2-implementer` — boot context `.nanite/agents/tether-sprint-2-implementer.md`

To boot: in a fresh Claude Code session at this repo, say
`Boot tether-sprint-1-implementer`. The boot process reads `.nanite/config.yaml`,
loads the listed skills (from `~/.nanite/skills/`), and reads the per-agent
context file.

## Sprint discipline (applies to all v0.6 work)

- **Branch off `main`**, FF-merge at sprint close, delete branch. No working
  on `chore/*` or other unrelated branches.
- **`make check` green before every commit that closes a task**, no exceptions.
- **Per-task subagent dispatch with `isolation: "worktree"`** for parallel
  work that touches separate areas.
- **Commit incrementally** with `feat(registry): T-vXXX-NN-MM — <brief>`.
  End commit messages with:
  ```
  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
  ```
- **Locked decisions don't reopen.** D1-D18 in v060-01 and D1-D8 in v060-02
  are final. Send a `request` to `agridd-keeper` before working around any
  of them.

## Importer-scope discipline (security-critical, applies to v060-02)

Cerberus catalog YAMLs commonly contain plaintext OAuth tokens
(`CLAUDE_CODE_OAUTH_TOKEN` and similar in `resources[].config.env`). The
v060-02 cerberus importer MUST extract identity-only fields. **It never
copies `resources[]`, `config`, `env`, or any operational content into the
registry.** Sync (from v060-01) inherits the same discipline — D18 dropped
the `cached_payload_json` column to enforce this at the schema level. Source
files remain the truth; the registry holds thin identity + a callback URI.

## Cross-references

- **`AGENTS.md`** — full project orientation, domain concepts, naming notes.
- **`docs/adr/`** — ADR archive.
- **`docs/api/README.md`** — HTTP/UDS daemon API reference.
- **`planning/docs/epics/v0.6-federation-directory.md`** — current active
  epic.
- **`planning/docs/roadmap.md`** — version-level roadmap.
