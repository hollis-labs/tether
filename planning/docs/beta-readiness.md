# Tether Beta Readiness — Install & Onboarding Assessment

**Date:** 2026-05-29
**Scope:** Install story, GUI settings UX, first-run setup wizard, and provider
path detection for the public/private beta. Feature-completeness of the core
control plane (sessions, registry, messaging, AI routing) is assumed solid and
is **not** re-litigated here.
**Reference baseline:** `hollis-labs/apps/hadron` — its install + onboarding
patterns are the bar we want to meet.

> **Verification status (2026-06-05):** every factual claim below was re-audited
> against the tree (last commit `00314b6`, which predates this doc — nothing has
> shipped against these gaps yet). All P0 gaps confirmed open. Three claims were
> corrected from the original 2026-05-29 draft: the §7.1 table's `mux init`
> seeding cell (no such command exists), §8.2 gap #3 (MCP errors *are* captured
> at the data layer — UI-surfacing gap only), and §8.3 PTY (no deprecation marker
> in code yet). The sprint of record is
> `planning/docs/sprints/v06x-01-beta-onboarding.md`.
>
> **Sprint v06x-01 shipped (2026-06-05):** all four P0 gaps closed. See §10 for
> the shipped-items checklist.

---

## 1. Verdict

The **runtime is beta-solid; the install and first-run experience is not.** A
new user today can `brew install` the CLI, but then hits a manual,
undocumented-by-the-product cliff: they must hand-create `~/.tether/catalog/`,
copy example YAMLs, and know which provider `command` paths to fill in. There
is no setup wizard, the GUI is not shipped in releases, and the most
mistake-prone settings (filesystem paths, binary paths) are freeform text with
no validation or detection.

Four gaps stand between us and a clean beta:

1. **No first-run bootstrap / setup wizard** — catalog creation is a manual
   `cp -R` documented in `docs/install.md`, not something the product does.
2. **GUI is not in the release** — `scripts/release.sh` bundles only `mux` and
   `mux-apikey-helper`; the sysop UI binary ships nowhere.
3. **Path settings are freeform + unvalidated** — provider `command`, state/
   workspace/temp roots are plain text inputs with no existence check, no
   browse, no auto-detect.
4. **Auto-detection exists in the library but is never surfaced** — the
   plumbing to find `claude`/`codex`/`opencode` on PATH is there; nothing in
   the CLI or GUI offers it to a user.

---

## 2. What we have today

### 2.1 Install / packaging (partial)

| Capability | Status | Location |
|---|---|---|
| Homebrew tap install | ✅ documented | `README.md`, `scripts/render-homebrew-formula.sh` |
| Release tarballs (darwin/linux × arm64/amd64) | ✅ | `scripts/release.sh`, `make package-release VERSION=…` |
| `make install` (PREFIX/BINDIR/DESTDIR) | ✅ | `Makefile:30` |
| `make go-install` (dev) | ✅ | `Makefile:36` |
| Homebrew formula rendering from checksums | ✅ | `scripts/render-homebrew-formula.sh` |
| SQLite auto-migration on first daemon start | ✅ (18 migrations) | `internal/store/`, `internal/app/service.go` |
| Install docs incl. "First-Time Setup" | ✅ manual steps | `docs/install.md` |
| CI quality gate (`make check`) | ✅ | `.github/workflows/ci.yml` |

**Gaps in this layer:**
- **Sysop UI not packaged.** `release.sh` only tars `mux` + `mux-apikey-helper`.
  The `release-build` / `release-install` make targets build sysop, but the
  tarball/Homebrew path does not include it. (Commit `d28ba20` aligned install
  paths but did not get sysop into the release artifacts.)
- **No release automation.** Only `ci.yml` exists (runs `make check`). There is
  no tag-triggered `release.yml`. Hadron has `release.yml` that publishes
  tarballs + checksums on tag — worth mirroring.
- **First-run is implicit and fragile.** The daemon will fail to start if
  `~/.tether/catalog/global.yaml` or the state DB path is missing. Catalog
  creation is a documented `install -d … && cp -R examples/catalog/*` — the
  product never does it for the user.

### 2.2 GUI — sysop UI (solid app, no onboarding)

- **Stack:** React 19 + Vite 5 + Tailwind 4, `@hollis-labs/sysop-ui` kit,
  served by Go binary `tether_sysop` on `:8947`, frontend embedded via
  `//go:embed`. Source: `apps/sysop/`.
- **8 pages:** Overview, Operations (home), Messaging, MCP, AI, Activity,
  Registry, Settings.
- **Settings page (6 tabs):** Setup (read-only path table), MCP, System
  (daemon config edit via `GlobalSettingsDialog`), Launches, Providers
  (full CRUD via `ProviderDialog`), Roadmap.
- **Provider editing exists:** ID, type, brand, runtime kind, **command (text)**,
  args, adapter (claude/codex/none), env mode, passthrough/redact.
- **Safety already present:** timestamped `*.bak-*` backups before overwrite/
  delete; runtime-drift warnings; server-side launch preview/diff.

**Gaps in this layer:**
- **No onboarding/wizard/empty-state flow.** App lands on Operations and
  assumes the user already understands the catalog model.
- **Path/command fields are freeform text** with no validation, no existence
  check, no native browse, no auto-detect. (See §2.3.)
- **No "required vs optional / set later" framing** anywhere.

### 2.3 Provider path config & detection (capable backend, no UX)

- Provider `command` lives in `~/.tether/catalog/providers/*.yaml`. Examples
  shipped for: claude-code, claude-goprovider, claude-pty, codex-cli,
  codex-app-server, opencode, api-stub (`examples/catalog/providers/`).
- **Auto-detection already works at the adapter layer:**
  - `cli-goprovider` (claude/codex): empty `command` → `$CLAUDE_CLI_PATH` /
    `$CODEX_CLI_PATH` then `exec.LookPath`.
  - opencode: empty `command` → `exec.LookPath("opencode")`.
  - Catalog-declared `command` always wins (multi-tenant safe; `PlanScopedAdapter`).
- **Validation is structural only** (`internal/config/validate.go`): `cli` type
  requires non-empty `command`; no check that the binary actually exists on disk.

**Gaps in this layer:**
- No detection surfaced to the user — no CLI `tether detect` / `tether doctor`,
  nothing in the GUI that says "found claude at /opt/homebrew/bin/claude".
- No on-disk existence validation at config-save or load time, so a typo'd path
  fails only at launch.

---

## 3. What we need for beta (prioritized)

### P0 — must-have for a credible beta

1. **First-run setup: `mux init` (CLI) + GUI Setup Wizard.**
   - `mux init` (idempotent): create `~/.tether/{catalog,state,run,...}`, seed
     `global.yaml` + example provider/agent/project YAMLs with sensible
     defaults, run migrations. Safe to re-run.
   - GUI wizard (mirror hadron's `BlueprintWizardPage` step pattern — sidebar
     step nav + linear flow + Review step). Proposed steps:
     1. Welcome / detected environment
     2. **Detect agents** — auto-find claude/codex/opencode; per row: detected
        path (editable), "paste path", "browse" — or **Skip ("You can set this
        later in Settings → Providers")**
     3. State location (defaults to `~/.tether`, advanced override)
     4. Optional: AI provider keys (skippable)
     5. Review + write catalog
   - Every non-required step gets an explicit **"Skip — set later in Settings"**
     affordance.

2. **Auto-detect claude/codex/opencode and surface it.**
   - Add `mux detect` / fold into `mux doctor`: run `exec.LookPath` for each
     known brand, report found/missing + resolved path. Reuse existing adapter
     `Detect()` plumbing.
   - Wire the same detection into the wizard step 2 and the Provider dialog
     ("Detect" button next to the command field).

3. **Ship the GUI in releases.**
   - Add sysop to `scripts/release.sh` tarballs (or a separate `tether-sysop`
     archive) and to the Homebrew formula.
   - Add a tag-triggered `.github/workflows/release.yml` (mirror hadron) so
     beta builds are reproducible and published.

4. **Validate the high-risk settings (kill freeform footguns).**
   - Path fields (state/workspace/temp roots, provider `command`): on
     save/blur, check existence + (for command) executability; show inline
     red/green status instead of silent acceptance.
   - Add **Browse** (native file/dir picker) to path fields. NOTE: sysop is a
     served web app, not Wails like hadron — native pickers need either a
     daemon-side `/api/fs/browse` endpoint or an Electron/Wails shell decision
     (see Open Questions).
   - Typed inputs where the value is an enum (already done for several) and
     duration/number validation where freeform today.

### P1 — strongly desired

5. **`mux doctor`** — one command that checks: daemon reachable, catalog valid,
   migrations current, each provider binary resolvable, socket/permissions
   sane. This is the single best "is my install healthy?" beta tool.
6. **Empty-state guidance in the GUI** — when no providers/launches/sessions
   exist, nudge into the wizard or a "Getting Started" panel (hadron dashboard
   pattern).
7. **Required-vs-optional metadata** on settings so the GUI can render "set
   later" consistently rather than ad hoc.

### P2 — polish

8. Help page with copy-paste example catalog snippets + "Create" buttons
   (hadron `HelpPage` pattern).
9. Bundle starter example catalog into the binary (`//go:embed`) so `mux init`
   needs no repo checkout.
10. Diff previews beyond launches (already noted as a sysop follow-up).

---

## 4. Decisions locked (2026-05-29)

These were decided during the assessment session and constrain the P0 build:

1. **File browse: detect + paste only for beta.** sysop is a browser-served SPA
   (not Wails), so native pickers aren't free. We ship auto-detect + manual
   paste; no Browse button and no daemon-side fs API for beta. Browse is a
   post-beta follow-up.
   *Why:* don't block beta on a desktop-shell or new fs-listing decision.
2. **Setup wizard is CLI-only (`mux init`).** No GUI wizard for beta. The GUI
   still gets the settings-validation pass, but the guided first-run flow lives
   entirely in the CLI.
   *Why:* fastest to ship, works headless/over SSH, no GUI dependency.
3. **Catalog bootstrap: both auto-seed + `mux init`.** The daemon self-seeds a
   minimal working catalog if none exists (so it never hard-fails on a missing
   catalog), and `mux init` performs the full guided setup (detect agents,
   choose state location, optional keys, write full example catalog).
   *Why:* friendly default + predictable explicit path.

---

## 5. Open question still to confirm

- **Scope of "important settings"** for the validation pass — assumed
  high-risk set is: provider command paths, state/workspace/temp roots, AI
  provider keys/base URLs. Confirm nothing else needs typed/validated input.

---

## 6. Suggested next step

Turn §3 P0 into a small sprint (e.g. `v06x-beta-onboarding`), now scoped by the
§4 decisions:

- `mux init` — guided CLI setup (detect → edit/paste → skip-with-"set later" →
  write full catalog). **CLI-only.**
- Daemon auto-seed of a minimal catalog when none exists.
- `mux detect` / `mux doctor` — surface adapter `Detect()` results +
  install-health checks.
- Release packaging: add sysop to tarballs/formula + tag-triggered
  `release.yml`.
- GUI settings-validation pass: existence/executability checks + inline
  red/green on path and command fields (**no Browse for beta**).

---

## 7. Configuration & control surfaces — providers / MCP / tools / routes

Beta goal (user, 2026-05-29): **don't make each surface feature-complete —
expose enough settings/controls that users see where we're headed and we get
real usage on these surfaces from day one.** Breadth over depth; polish passes
come after beta features lock in.

### 7.1 Current add/manage model (inconsistent — this is the gap)

| Kind | Seeded on install? | Known-list to pick from? | "Add new" UX today | Persisted to |
|---|---|---|---|---|
| **AI providers** | ✅ in `global.yaml` | ✅ **real** — vendor types (anthropic/openai/openai-compatible/gemini) + live model catalog via `modelsdev` | Form w/ **vendor dropdown + model picker** | `global.yaml` `ai.providers[]` |
| **AI routes** | ✅ in `global.yaml` | partial — provider dropdown from configured providers | Form w/ provider/model/mode/intent | `global.yaml` `ai.routing.routes[]` |
| **CLI providers** | ❌ **not seeded** — no `mux init` exists yet (manual `cp -R examples/` only, per `docs/install.md`) | ❌ implicit only — brand inferred by ID prefix (`claude-*`/`codex-*`/`opencode`) in `internal/config/provider_runtime.go:20-39` | **blank form** | per-file `providers/<id>.yaml` |
| **MCP servers** | ❌ **not seeded** | ❌ none | **blank form** | per-file `mcp-servers/<id>.yaml` |

**The story today:** partial seeding + mostly blank forms. The exact pattern
you described — *seed a known list, user clicks Add, picks a type, then
configures* — already works for AI providers (model picker). It does **not**
exist for CLI providers (blank form, brand guessed from the ID string) or MCP
servers (not even seeded).

### 7.2 The unifying target (direction; mostly post-beta)

Introduce a small **embedded "known templates" registry** (Go, `//go:embed` or
a typed table) of recognized types, and make every "Add" flow:
`Add → pick from known list → prefilled form → configure → save`.

- **CLI providers:** known templates for `claude-code`, `codex`, `opencode`
  (prefilled command/adapter/runtime-kind/env defaults). Replaces ID-prefix
  brand guessing with an explicit picked type.
- **MCP servers:** a starter set of recognized servers + a "custom (blank)"
  option. Even a handful (filesystem, git, fetch, plus the tether-native one)
  gives users a working pick-and-go experience.
- **AI providers:** already there — generalize the same component shape.

This makes the four surfaces consistent and is the right home for "where we're
headed." **Per your call, this does not have to change for beta** — it's the
documented direction.

### 7.3 Beta scope for these surfaces (breadth, not depth)

- **CLI providers:** keep current CRUD; **seed `mux init` with the 3 known
  provider YAMLs** so there's always a working starting point (today only
  examples are copied). Add a "type" hint in the Add dialog even if it just
  prefills defaults — low effort, shows direction.
- **MCP servers:** **seed at least 1–2 example MCP server YAMLs** (currently
  zero) so the page isn't empty on first run; keep blank-form Add.
- **AI providers/routes:** already in good shape — no beta change needed beyond
  the validation pass (§3).
- **Tools / broker:** see §7.4 — surface a read-mostly control view.

### 7.4 Tools & broker controls (expose the knobs that already exist)

There is no single "tool access broker" today — "broker" is two distinct things
(skill-ranking broker + inter-agent message broker), neither of which gates
tools. Tool controls that **already exist** but are scattered across catalog
YAML:

- MCP server **enable/disable** (`enabled:` + `/api/mcp/servers/toggle`).
- MCP **visibility allowlist** per project/launch (`mcp.servers: [...]`).
- MCP **scope gates** (`scopes: [session.write, ai.invoke, ...]`).
- AI **`allow_tools`** policy (global + per-route) at the AI gateway.
- Agent **permission mode** (`bypass`/`default`).

**Beta move (read + light edit, decided 2026-05-29):** a single **"Tools &
Broker"** view that surfaces these existing knobs in one place AND makes the
cheap ones editable in the GUI:

- **Editable in beta:** per-server enable/disable (already), **MCP visibility
  allowlists** (project/launch `mcp.servers[]`), and **scope grants**
  (`scopes[]`). These are catalog-YAML edits we already know how to write
  safely (the providers/MCP save paths + `*.bak-*` backups exist).
- **Visible, not editable in beta:** AI `allow_tools` already editable on the
  AI page; surface it here read-only for context.
- **Labeled "coming soon":** per-tool enable/deny + deny-lists (no per-tool
  policy engine for beta).

This gives real control at beta (allowlists + scopes are the knobs operators
actually reach for) while deferring the per-tool policy engine.

---

## 8. Operator visibility — logs & errors

You're right that we're short here. Some surfaces are solid; the highest-value
debugging paths have real holes.

### 8.1 What's solid today

- **AI audit** (`/ai` → Audit, `ai_events` table): structured per-call records
  with success/error text, refusal, latency, tokens, cost. Good.
- **Activity Monitor** (`/activity`): event stream + tool-call aggregates
  (calls, errors-count, success%, p95) from `proxy_events`.
- **MCP tools page**: merged live-probe + usage history, per-tool error counts.
- **Session detail dialog**: lifecycle state, PID, exit code, attachments,
  checkpoints.
- **Typed API error envelope** (ADR 0010): consistent `{code, message}`.

### 8.2 The gaps (ranked)

| # | Gap | Why it hurts an operator | Severity |
|---|---|---|---|
| 1 | **Daemon logs go to `/dev/null`** — stderr is set `nil` on spawn (`cmd/mux/daemon.go`) | Can't see *why* the daemon failed to start (registry/AI-gateway init errors are logged then lost) | **HIGH** |
| 2 | **Session output not persisted for subprocess / streaming runtimes** — runtime output isn't captured to the store | Can't review session output / crash messages after it ends | **HIGH** |
| 3 | **MCP server connection errors captured but not surfaced** — `ServerStatus.Error` already holds the reason (`internal/mcpadapter/client_pool.go:285,299`); the servers page only shows the "failed" state, not the text | Can't see *why* a server failed (e.g. command not found) — UI-only gap, data already exists | LOW-MED |
| 4 | **Tool error text stored but not shown** in tool-calls table (in `proxy_events`, needs row drill-in) | Errors are countable but not readable at a glance | MED |
| 5 | **No unified errors view** — errors scattered across `/ai`, `/activity`, `/mcp` | No "what's broken in the last 24h" rollup | MED |

### 8.3 Beta scope for logs/errors

**P0 (high value, modest effort):**
- **Daemon → log file.** Write `muxd` logs to `~/.tether/logs/muxd.log`
  (instead of `/dev/null`) with basic size-based rotation. Mirrors hadron's
  `~/.hadron/logs/`. This single change fixes the worst blind spot.
- **GUI "Daemon Logs" tail.** A read-only log-tail view (`/api/logs/daemon`
  serving the file). Closes gap #1 in the UI.
- **Surface MCP server error/last-failure** on the servers page (gap #3) and
  **show tool error text** in the tool-calls table (gap #4 — data already
  exists, UI-only change).

**P1:**
- An **"Errors" filter/rollup** across the activity feed (gap #5).
- Consider **structured logging (slog/JSON)** for the daemon while we're
  touching the log path — better than `log.Printf` for an errors view.

**Scope correction (2026-05-29):** PTY is **not** a user-facing runtime going
forward — **subprocess and streaming(-stdio / jsonrpc-stdio) are the primary
runtimes** and the only ones output-capture needs to target. This makes gap #2
more tractable than "capture a PTY": for subprocess/streaming the output is a
plain stream we can tee → store.

> **Code status (verified 2026-06-05):** the PTY runtime still fully exists —
> `internal/app/runtime_resolver.go:36-37`, `newClaudePTYRuntime()` — with **no
> deprecation marker in code**. This is a forward-looking decision, not yet
> enforced. The beta sprint should add a guard/marker so the deprecation is
> real, not just asserted.

- **Beta P1:** capture subprocess + streaming-stdio/jsonrpc-stdio session
  output → store, viewable in the session detail dialog.
- **Explicitly out of scope:** PTY output capture (PTY is not the product
  direction).

---

## 9. Consolidated beta scope (config + observability addendum)

On top of §3 (install/onboarding), the beta needs these to satisfy the
"expose the surfaces + let operators debug" goal:

**P0**
- Seed `mux init` with the 3 known CLI-provider YAMLs + 1–2 example MCP servers
  (no empty pages on first run).
- Tools & Broker control view — **read + light edit**: MCP enable/disable +
  visibility allowlists + scope grants editable; AI `allow_tools` shown;
  per-tool policy labeled "coming soon."
- Daemon logs → file + GUI log-tail view.
- Surface MCP server error + tool error text in existing pages.

**P1**
- Embedded "known templates" registry + unified `Add → pick → configure` flow
  (the §7.2 direction; AI providers already model it).
- **Subprocess + streaming session-output capture → store**, viewable in
  session detail (PTY explicitly excluded).
- Errors rollup across activity; structured daemon logging.

**Known limitations to record**
- Per-tool policy / deny-lists not in beta (controls are server/scope-level).
- PTY session output not captured (PTY is not a user-facing runtime).

---

## 10. Sprint v06x-01 shipped items (2026-06-05)

All P0 gaps from §3 are closed. Checklist against sprint exit criteria:

- [x] `mux init` exists, idempotent, guided setup from nothing to working `~/.tether/`.
- [x] Daemon self-seeds a minimal working catalog; never hard-fails on missing `global.yaml`.
- [x] `mux detect` reports found/missing + resolved path for claude / codex / opencode.
- [x] `mux doctor` checks daemon, catalog, migrations, provider binaries, permissions.
- [x] Daemon logs written to `~/.tether/logs/muxd.log` (size-rotated); sysop Daemon Logs tail view ships.
- [x] `tether_sysop` in release tarballs and Homebrew formula; tag-triggered `release.yml` publishes.
- [x] Provider `command` + path fields validate existence/executability on blur; inline red/green status.
- [x] Starter catalog seeds 3 CLI providers + 1 MCP server (no empty first-run pages).
- [x] MCP server failure reason (`ServerStatus.Error`) surfaced on MCP page.
- [x] Per-tool error text surfaced in Activity Monitor tool-calls table.
- [x] "Tools & Broker" sysop view: MCP enable/disable + scope grants editable; visibility read-only; per-tool policy "coming soon".
- [x] PTY runtime carries deprecation comment + log-warn (`internal/app/runtime_resolver.go`); not removed.
- [x] ADR `0044-beta-onboarding-and-install.md` records D1–D9 and deferred items.
- [x] `docs/install.md` leads with `mux init`; `mux detect`/`mux doctor` documented.
- [x] README quick-start mentions `mux init` + shipped sysop UI.
- [x] `make check` green.

**Remaining P1 (not in this sprint):**
- GUI setup wizard (D2).
- Native Browse pickers (D1).
- Embedded known-templates registry (D9).
- Subprocess + streaming session-output capture → store.
- Per-tool policy engine / deny-lists (D6).
- Errors rollup; structured (slog/JSON) daemon logging.
