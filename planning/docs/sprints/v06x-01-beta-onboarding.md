# Sprint v06x-01 — Beta Onboarding & Install Story

**Assessment of record:** [`planning/docs/beta-readiness.md`](../beta-readiness.md) (audited 2026-06-05)
**Scope:** Close the four install/first-run gaps + the two highest-value
observability gaps that stand between the (beta-solid) runtime and a credible
public/private beta. Make a new user go from `brew install` to a working,
self-explaining install without hand-editing YAML, and give operators enough
log/error visibility to debug their own setup.
**Target duration:** ~8–10 business days.
**Reference baseline:** `hollis-labs/apps/hadron` — its `mux init`-equivalent,
`~/.hadron/logs/`, and tag-triggered `release.yml` are the bar.

**Non-goals (explicitly deferred to a P1 follow-up sprint):** GUI setup wizard,
native Browse pickers, embedded "known-templates" Add→pick→configure registry,
subprocess/streaming session-output capture, per-tool policy engine, errors
rollup view, structured (slog/JSON) daemon logging. See §"Deferred (P1+)".

---

## Exit criteria

- [ ] `mux init` exists, is idempotent, and takes a fresh machine from nothing
      to a working `~/.tether/` (catalog + state + run + logs dirs, full example
      catalog written, migrations applied, agents auto-detected with skip-able
      "set later" affordance).
- [ ] Daemon self-seeds a minimal working catalog when none exists — it never
      hard-fails on a missing `~/.tether/catalog/global.yaml`.
- [ ] `mux detect` reports found/missing + resolved path for claude / codex /
      opencode, reusing the existing adapter `Detect()` plumbing.
- [ ] `mux doctor` checks: daemon reachable, catalog valid, migrations current,
      each provider binary resolvable, socket + state-dir permissions sane —
      and prints an actionable pass/fail report with a non-zero exit on failure.
- [ ] Daemon logs are written to `~/.tether/logs/muxd.log` (size-rotated), not
      `/dev/null`. A read-only "Daemon Logs" tail view exists in sysop.
- [ ] The sysop binary (`tether_sysop`) ships in the release tarballs **and**
      the Homebrew formula. A tag-triggered `.github/workflows/release.yml`
      publishes tarballs + checksums.
- [ ] Provider `command` + state/workspace/temp path fields validate existence
      (and executability for `command`) on save/blur, showing inline red/green —
      no silent acceptance of typos. No Browse button (detect + paste only).
- [ ] Example catalog seeds the 3 known CLI providers **and** ≥1 MCP server, so
      no first-run page is empty.
- [ ] MCP server failure reason (`ServerStatus.Error`) and per-tool error text
      (`proxy_events.error`) are surfaced in their existing sysop pages.
- [ ] A single "Tools & Broker" sysop view surfaces the scattered tool knobs;
      MCP enable/disable + visibility allowlists + scope grants are editable
      there; AI `allow_tools` shown read-only; per-tool policy labeled "coming
      soon."
- [ ] PTY runtime carries a deprecation marker in code (not user-facing).
- [ ] ADR `0044-beta-onboarding-and-install.md` records the locked decisions.
- [ ] `make check` green (`fmt + vet + lint + test-race + vuln + coverage`).

---

## Decisions locked (don't reopen in this sprint)

Carried from `beta-readiness.md` §4 + the 2026-06-05 audit corrections.

- **D1 File browse: detect + paste only.** sysop is a browser-served SPA (not
  Wails), so native pickers aren't free. Ship auto-detect + manual paste; no
  Browse button, no daemon-side `/api/fs/browse` for beta. Browse is post-beta.
- **D2 Setup wizard is CLI-only (`mux init`).** No GUI wizard for beta. The GUI
  gets the settings-validation pass (T-07) but the guided first-run flow lives
  entirely in the CLI. Rationale: fastest to ship, works headless/over SSH.
- **D3 Catalog bootstrap = both auto-seed + `mux init`.** The daemon self-seeds
  a minimal working catalog if none exists (never hard-fails); `mux init`
  performs the full guided setup (detect → edit/paste → skip-with-"set later" →
  write full example catalog). Both consume the same embedded catalog source.
- **D4 "Set later" is a first-class affordance.** Every non-required `mux init`
  step gets an explicit skip that prints where to set it later (Settings →
  Providers, etc.). Skipping never blocks completion.
- **D5 Validation is existence + executability only.** Path fields check the
  path exists; `command` additionally checks the file is executable. No deep
  semantic validation, no "run it and see." Validation runs daemon-side (the
  daemon owns the filesystem the binaries live on) and the GUI renders the
  result. Save is **not** blocked on a red status (warn, don't gate) — a user
  may legitimately configure a path that doesn't exist yet.
- **D6 Tools & Broker scope.** Editable in beta: MCP per-server enable/disable,
  MCP visibility allowlists (`mcp.servers[]`), scope grants (`scopes[]`). Shown
  read-only: AI `allow_tools` (already editable on the AI page). Labeled "coming
  soon": per-tool enable/deny + deny-lists. No per-tool policy engine in beta.
- **D7 PTY is not a user-facing runtime.** subprocess + streaming(-stdio /
  jsonrpc-stdio) are the primary runtimes. PTY gets a deprecation marker this
  sprint; PTY output capture is explicitly out of scope. PTY is **not** removed
  in this sprint (still resolvable) — only marked.
- **D8 No raw-content leakage in seeds.** Seeded example provider/MCP YAMLs
  carry empty `command` (rely on adapter auto-detect) and **no** secrets/tokens
  in `env`. The same importer-scope discipline as v060-02 applies: seeds are
  identity/structure only.
- **D9 Embedded known-templates registry is post-beta.** The §7.2 "Add → pick
  from known list → prefilled form" direction is P1, not this sprint. Beta seeds
  the YAMLs so pages aren't empty; the Add flow stays as-is (blank form + the
  ID-prefix brand inference already in `provider_runtime.go`).

---

## Tasks

### T-v06x-01-01: Setup foundation — detection helper + embedded starter catalog

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [beta, onboarding, foundation, detection, embed]

#### Problem

`mux init`, the daemon auto-seed, `mux detect`, and `mux doctor` all need two
shared primitives that don't exist yet as reusable surfaces: (1) a detection
helper that resolves claude/codex/opencode without standing up a full launch
plan, and (2) an embedded copy of the example catalog so seeding needs no repo
checkout. Build them once here; the consumer tasks depend on this.

#### Fix direction

- New `internal/setup/` package (`doc.go` documenting it as the shared
  onboarding substrate).
- **Detection helper** `setup.DetectProviders() []DetectResult` where
  `DetectResult = {Brand string, Found bool, Path string, Source string}`.
  - Reuse the existing adapter `Detect()` plumbing — `claudestream`/`codex`
    inner-adapter `$CLAUDE_CLI_PATH`/`$CODEX_CLI_PATH` → `exec.LookPath`, and
    `opencode`'s `exec.LookPath("opencode")` (`internal/provider/cli/*/cliadapter.go`).
    Do **not** duplicate the lookup logic — call the existing `Detect()` entry
    points (or factor a tiny shared `lookup(brand)` they both use).
  - `Source` records how it was found (`env:CLAUDE_CLI_PATH` / `PATH` / unset).
- **Embedded catalog** via `//go:embed`. Add a `embed.go` that embeds the
  on-disk `examples/catalog/**` tree (or a curated `internal/setup/seed/` copy —
  pick one; if embedding `examples/`, ensure the path resolves from the module
  root). Expose `setup.WriteCatalog(dst string, opts WriteOpts) (WriteReport, error)`:
  - Creates `dst/{catalog,state,run,logs}` and the catalog subdirs.
  - Writes every embedded YAML, skipping any file that already exists unless
    `opts.Force` (idempotent — safe to re-run).
  - `opts.Minimal` writes only the minimum to boot (`global.yaml` + one example
    provider) for the daemon auto-seed path; full mode writes everything.
  - Honors `opts.ProviderCommands map[brand]string` to stamp detected paths into
    the seeded provider YAMLs (empty = leave blank for adapter auto-detect).
- **Catalog content additions** (so seeds aren't half-empty — see T-06 too):
  - Confirm `examples/catalog/providers/` has working `claude-code`, `codex-cli`,
    `opencode` YAMLs with **empty `command`** (per D8).
  - Add `examples/catalog/mcp-servers/` with ≥1 example server YAML (e.g. a
    filesystem or fetch server) — this directory does not exist today.

#### Files

- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/setup/doc.go` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/setup/detect.go` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/setup/seed.go` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/setup/embed.go` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/examples/catalog/mcp-servers/*.yaml` (new)
- `/Users/chrispian/dev/hollis-labs/apps/tether/examples/catalog/providers/*.yaml` (verify/extend)

#### Acceptance criteria

- [ ] `DetectProviders()` returns one row per known brand; with a fake PATH
      containing a stub `claude`, the row is `Found:true` with the resolved path.
- [ ] `WriteCatalog` on an empty temp dir produces a catalog the daemon can boot
      from (assert via `config.LoadLayered` succeeding on the written tree).
- [ ] `WriteCatalog` is idempotent — second run reports 0 written / N skipped;
      `Force:true` overwrites with `*.bak-*` backups (reuse the sysop backup
      convention).
- [ ] `ProviderCommands` stamps a detected path into the right provider YAML.
- [ ] Embedded MCP-server example present; `config.LoadLayered` parses it.
- [ ] `make test-race` green.

#### Scope fences

- No CLI command, no daemon wiring, no GUI in this task — pure library.
- Do not build the embedded "known-templates" picker registry (D9, post-beta).

---

### T-v06x-01-02: Daemon auto-seed minimal catalog when none exists

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [beta, onboarding, daemon, auto-seed]
**depends_on:** T-v06x-01-01

#### Problem

`internal/app/service.go:85-92` calls `config.LoadLayered(catalogRoot)`, which
hard-fails if `~/.tether/catalog/global.yaml` is missing. A first-run daemon (no
`mux init` yet) dies instead of coming up usable. Per D3 the daemon must
self-seed a minimal catalog so it never hard-fails on a missing catalog.

#### Fix direction

- In `app.New` (or the daemon-run path that calls it), before `LoadLayered`:
  if the catalog root is missing or has no `global.yaml`, call
  `setup.WriteCatalog(root, WriteOpts{Minimal:true})`, then proceed.
- Log a single clear line: `catalog absent — seeded minimal catalog at <root>;
  run 'mux init' for guided setup`.
- Do **not** auto-seed over an existing catalog (only the empty/missing case).
- Auto-seed must be quiet and fast (it's the minimal set), and must leave the
  full guided experience to `mux init`.

#### Acceptance criteria

- [ ] Fresh temp `$TETHER_STATE`/catalog-root with no files → daemon starts,
      catalog present afterward, listener binds.
- [ ] Existing catalog → auto-seed is a no-op (no files touched, no backups).
- [ ] The seed log line is emitted exactly once on the seeded path.
- [ ] `make test-race` green.

#### Scope fences

- No interactive prompts (this is the non-interactive safety net).
- Do not run detection here — auto-seed writes blank-command providers; detection
  is `mux init` / `mux detect`'s job.

---

### T-v06x-01-03: `mux init` — guided CLI first-run setup

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [beta, onboarding, cli, init]
**depends_on:** T-v06x-01-01

#### Problem

There is no `mux init` command (`cmd/mux/root.go` command list confirms). First
run is a manual `install -d … && cp -R examples/catalog/*` documented in
`docs/install.md` — the product never does it. Per D2 the guided flow is
CLI-only for beta.

#### Fix direction

- New `cmd/mux/init.go`, registered in `root.go`. Idempotent and re-runnable.
- Flow (linear, matches `beta-readiness.md` §3-P0.1, minus the GUI):
  1. **Welcome / detected environment** — print state root (`~/.tether`,
     overridable via `--state-dir` / `$TETHER_STATE`).
  2. **Detect agents** — call `setup.DetectProviders()`. Per brand, show the
     detected path; let the user accept, paste an override, or **skip ("set
     later in Settings → Providers")** per D4. Non-TTY/`--yes` mode auto-accepts
     detected paths and skips undetected ones.
  3. **State location** — default `~/.tether`, advanced override.
  4. **Optional AI provider keys** — skippable; if provided, written to
     `global.yaml` `ai.providers[]` (or env reference — do not write raw keys to
     a world-readable file without 0600 perms).
  5. **Review + write** — show what will be written, then
     `setup.WriteCatalog(root, {Force:false, ProviderCommands: <accepted>})`,
     apply migrations, print next steps (`mux doctor`, `mux daemon start`).
- Flags: `--state-dir`, `--yes` (non-interactive accept-detected), `--force`
  (overwrite with backups), `--print-plan` (dry-run).
- Re-run on an existing install: detect what's present, only fill gaps, never
  clobber without `--force`.

#### Acceptance criteria

- [ ] `mux init --yes` on an empty temp state dir produces a bootable catalog;
      `mux doctor` (T-04) passes against it.
- [ ] Detected provider path lands in the seeded provider YAML; skipped brand
      leaves `command` empty and prints the "set later" pointer.
- [ ] Re-run is idempotent (no clobber without `--force`); `--force` backs up.
- [ ] `--print-plan` writes nothing.
- [ ] Non-TTY stdin does not hang (auto-`--yes` semantics or clean error).
- [ ] `--help` documents every step + the skip/"set later" behavior.
- [ ] `make test-race` green (drive interactive flow via injected reader/writer).

#### Scope fences

- No GUI wizard (D2). No Browse picker (D1).
- Do not re-implement detection or seeding — consume `internal/setup`.

---

### T-v06x-01-04: `mux detect` + `mux doctor`

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [beta, onboarding, cli, doctor, detect]
**depends_on:** T-v06x-01-01

#### Problem

No way to answer "is my install healthy?" or "did you find my agents?" from the
shell. The adapter `Detect()` results are never surfaced; there's no health
check. `beta-readiness.md` §3-P0.2 + §3-P1.5.

#### Fix direction

- New `cmd/mux/doctor.go` (hosts both subcommands or `doctor` + a `detect`
  sibling — keep `mux detect` and `mux doctor` as the two entry points).
- **`mux detect`** — call `setup.DetectProviders()`, print a table: brand /
  found? / resolved path / source. `--json` for machine output. Exit 0 always
  (it's informational).
- **`mux doctor`** — run an ordered set of checks, each `name → ok|warn|fail`
  with a one-line remedy on non-ok:
  - daemon reachable (UDS `~/.tether/run/muxd.sock` ping via the client).
  - catalog present + valid (`config.LoadLayered` succeeds).
  - migrations current (store reports no pending migrations).
  - each configured provider `command` resolvable + executable (or empty +
    adapter-detectable — call detection).
  - socket + state-dir permissions sane (dir exists, writable, sane mode).
  - logs dir exists + writable (ties to T-05).
  - `--json` for machine output; exit non-zero if any check is `fail`
    (`warn` does not fail the exit per D5's warn-don't-gate stance).

#### Acceptance criteria

- [ ] `mux detect` table + `--json` both correct against a stubbed PATH.
- [ ] `mux doctor` passes on a freshly `mux init`'d install; exit 0.
- [ ] Each failure mode (daemon down, missing catalog, unresolvable provider
      command, unwritable state dir) produces the right `fail`/`warn` line + a
      non-zero exit only on `fail`.
- [ ] `--json` shape is stable + documented in `--help`.
- [ ] `make test-race` green.

#### Scope fences

- `doctor` does not mutate anything — read-only diagnosis (no auto-fix in beta).
- Do not duplicate detection logic — reuse `internal/setup`.

---

### T-v06x-01-05: Daemon logs → file + sysop "Daemon Logs" tail

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [beta, observability, logs, sysop]

#### Problem

`cmd/mux/daemon.go:69-70` sets the re-exec child's `Stdout`/`Stderr` to `nil`,
so all daemon logs (registry/AI-gateway init errors included) go to `/dev/null`.
This is the single worst operator blind spot (`beta-readiness.md` §8.2 gap #1,
HIGH). No `~/.tether/logs/` is created anywhere.

#### Fix direction

- On daemon spawn, point the child's stdout+stderr at `~/.tether/logs/muxd.log`
  (open/create, append). Ensure the `logs/` dir exists (created by
  `setup.WriteCatalog` / auto-seed, but the daemon must also create it defensively).
- Basic **size-based rotation**: when `muxd.log` exceeds N MiB (e.g. 10 MiB),
  rotate to `muxd.log.1` (keep a small fixed number of generations). Keep it
  simple — no external rotation dep; a check-on-open + rename is fine.
- Mirror hadron's `~/.hadron/logs/` layout/behavior where reasonable.
- **sysop "Daemon Logs" view:** a read-only tail.
  - Daemon-side `GET /api/logs/daemon?tail=N` serving the last N lines of
    `muxd.log` (bounded; never stream the whole file unbounded).
  - A new sysop page or a tab under an existing diagnostics area showing the
    tail with a manual refresh (no live websocket required for beta).

#### Files

- `/Users/chrispian/dev/hollis-labs/apps/tether/cmd/mux/daemon.go` (edit)
- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/api/*` (new logs endpoint)
- `/Users/chrispian/dev/hollis-labs/apps/tether/apps/sysop/...` (tail view + route)

#### Acceptance criteria

- [ ] After `mux daemon start`, `~/.tether/logs/muxd.log` exists and receives
      daemon stdout/stderr (previously-lost init lines now visible).
- [ ] Oversized log rotates; generation count bounded.
- [ ] `GET /api/logs/daemon?tail=100` returns the last 100 lines, bounded; bad
      `tail` values are clamped, not unbounded.
- [ ] sysop tail view renders the log + refresh; empty-log state handled.
- [ ] `make check` green.

#### Scope fences

- No structured/slog/JSON logging in this task (P1 — keep `log.Printf` output,
  just redirect + rotate it).
- No live-streaming/websocket tail for beta — manual refresh is enough.

---

### T-v06x-01-06: Seed CLI providers + MCP servers (no empty first-run pages)

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [beta, onboarding, catalog, seed]
**depends_on:** T-v06x-01-01

#### Problem

`beta-readiness.md` §7.1/§7.3: MCP servers are seeded nowhere
(`examples/catalog/mcp-servers/` doesn't exist), and CLI providers are only
copied manually. On first run the MCP page is empty and providers depend on a
manual `cp`. Per D3 both `mux init` and auto-seed should leave a working,
non-empty starting point.

#### Fix direction

- Finalize the seeded set (the YAML content; T-01 wires the embed/write):
  - 3 CLI provider YAMLs: `claude-code`, `codex-cli`, `opencode` — empty
    `command`, correct `adapter`/`runtime` defaults, no secrets (D8).
  - 1–2 example MCP server YAMLs (e.g. filesystem + fetch) with safe defaults,
    `enabled: false` if running them needs a binary the user may not have.
- Confirm `mux init` (full) and auto-seed (minimal includes ≥1 provider) both
  produce non-empty Providers + (for full) MCP pages.
- In the provider Add dialog, add a lightweight **type hint** that prefills the
  command/adapter/runtime defaults for the picked brand (low-effort nod to the
  §7.2 direction) — but **not** the full known-templates registry (D9).

#### Acceptance criteria

- [ ] Fresh `mux init` → sysop Providers page shows the 3 CLI providers and the
      MCP page shows ≥1 server (not empty).
- [ ] Seeded YAMLs contain no tokens/secrets and parse via `config.LoadLayered`.
- [ ] Provider Add dialog brand selection prefills sensible defaults.
- [ ] `make check` green.

#### Scope fences

- No known-templates picker registry / `//go:embed` template table (D9).
- Do not seed providers with real binary paths — empty command + auto-detect.

---

### T-v06x-01-07: GUI settings validation — existence/executability + inline status

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [beta, sysop, validation, providers]

#### Problem

Provider `command` and state/workspace/temp path fields are freeform text with
no validation (`apps/sysop/.../settings.tsx:994-996`; backend
`providerFromSaveRequest` checks only catalog ID/config consistency). A typo'd
path fails silently at launch, not at save. `beta-readiness.md` §3-P0.4. Per D5
this is existence + executability only, warn-don't-gate, detect+paste (no Browse).

#### Fix direction

- Daemon-side validation endpoint (the daemon owns the FS the binaries live on):
  `POST /api/fs/validate` body `{path, kind: "file"|"dir"|"executable"}` →
  `{exists, executable?, resolved?, note?}`. Bounded, read-only `os.Stat` /
  exec-bit check; never executes the binary.
- Wire the provider `command` field and the path fields (state/workspace/temp
  roots) to call it on blur/save and render inline **red/green** status with the
  resolved path. For `command`, an empty value is **valid** (adapter
  auto-detect) — show a neutral "will auto-detect" hint, not red.
- Add a **"Detect"** affordance next to the provider command field that calls a
  daemon-side detect (reuse `setup.DetectProviders`, or a per-brand variant) and
  fills the resolved path — the GUI counterpart to `mux detect` (D1: detect +
  paste, no Browse).
- Save is **not** blocked on red (D5) — show the warning, allow save.

#### Files

- `/Users/chrispian/dev/hollis-labs/apps/tether/internal/api/*` (validate + detect endpoints)
- `/Users/chrispian/dev/hollis-labs/apps/tether/apps/sysop/frontend/src/pages/settings.tsx` (edit)

#### Acceptance criteria

- [ ] Valid existing path/binary → green + resolved path; missing → red + note;
      empty `command` → neutral "auto-detect" hint.
- [ ] "Detect" button fills the resolved claude/codex/opencode path.
- [ ] Validation endpoint never executes the target binary (exec-bit check only).
- [ ] Save succeeds even with a red field (warn-don't-gate).
- [ ] `make check` green.

#### Scope fences

- No Browse / native picker / `/api/fs/browse` listing endpoint (D1).
- No deep semantic validation beyond exists + executable (D5).

---

### T-v06x-01-08: Surface MCP server errors + tool error text

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [beta, observability, sysop, mcp]

#### Problem

The data exists but isn't shown. `ServerStatus.Error`
(`internal/mcpadapter/client_pool.go:285,299`) already holds the failure reason,
but the MCP servers page shows only the "failed" state (§8.2 gap #3 — corrected:
UI-only). Tool error text lives in `proxy_events.error` but isn't readable in the
tool-calls table (gap #4). Both are UI-surfacing changes, no new capture needed.

#### Fix direction

- MCP servers page: when a server is `failed`, show `ServerStatus.Error` (the
  reason — e.g. "command not found") inline or in a drill-in, not just the badge.
- Activity tool-calls table: surface the stored `proxy_events.error` text on the
  row (drill-in or expandable cell) so errors are readable at a glance, not just
  countable.
- Confirm the API responses already carry these fields to the frontend; extend
  the response shape only if a field is dropped server-side.

#### Acceptance criteria

- [ ] A deliberately-broken MCP server (bad command) shows its error reason in
      the servers page.
- [ ] A tool call that errored shows its error text in the activity table.
- [ ] No new capture/storage added — purely surfacing existing data.
- [ ] `make check` green.

#### Scope fences

- No unified "Errors" rollup view (P1 — `beta-readiness.md` §8.3 P1).
- No new tables/columns.

---

### T-v06x-01-09: "Tools & Broker" sysop view (read + light edit)

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [beta, sysop, tools, broker, scopes]

#### Problem

Tool controls exist but are scattered across catalog YAML and pages
(`beta-readiness.md` §7.4): MCP `enabled` + `/api/mcp/servers/toggle`, MCP
visibility allowlist (`mcp.servers[]`), scope gates (`scopes[]`), AI
`allow_tools`, agent permission mode. There's no single place to see/operate
them. Per D6, beta gets one view that surfaces all of them and makes the cheap
ones editable.

#### Fix direction

- New sysop "Tools & Broker" page + route (alongside the existing 8).
- **Editable (D6):** per-server MCP enable/disable (reuse existing toggle), MCP
  visibility allowlists (`mcp.servers[]` on project/launch), scope grants
  (`scopes[]`). These are catalog-YAML edits the providers/MCP save paths +
  `*.bak-*` backups already know how to do safely — reuse them; don't invent a
  new write path.
- **Read-only:** AI `allow_tools` (already editable on the AI page) shown here
  for context with a link to the AI page.
- **Labeled "coming soon":** per-tool enable/deny + deny-lists (no policy engine
  in beta).
- Make clear in the UI which knob lives where (scope gate vs visibility
  allowlist vs server enable) so operators understand the layering.

#### Acceptance criteria

- [ ] The page lists every server with its enabled state, visibility allowlists,
      and scope grants; AI `allow_tools` shown read-only.
- [ ] Editing an allowlist / scope grant / enable writes the catalog YAML with a
      `*.bak-*` backup and reloads correctly.
- [ ] "Coming soon" per-tool policy section is visibly disabled, not a dead form.
- [ ] `make check` green.

#### Scope fences

- No per-tool policy engine / deny-lists (D6 — coming-soon only).
- Reuse existing save/backup paths — no new persistence mechanism.

---

### T-v06x-01-10: Release packaging — ship sysop + tag-triggered `release.yml`

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [beta, release, packaging, ci, homebrew]

#### Problem

`scripts/release.sh:65` tars only `mux mux-apikey-helper README.md LICENSE
install.md` — the `tether_sysop` binary ships nowhere, even though
`release-build`/`release-install` (Makefile:50,52) build it.
`render-homebrew-formula.sh:74` installs only the two binaries. There's no
tag-triggered `release.yml` (only `ci.yml`). `beta-readiness.md` §3-P0.3.

#### Fix direction

- **`scripts/release.sh`:** build `tether_sysop` for each target
  (`apps/sysop/cmd/tether_sysop`, with the frontend embedded — invoke the same
  build the `sysop-build` make target uses) and include the binary in each
  tarball (or a parallel `tether-sysop_<ver>_<os>_<arch>.tar.gz` archive — pick
  one; bundling into the main tarball is simpler for a beta).
- **`scripts/render-homebrew-formula.sh`:** install `tether_sysop` alongside
  `mux` + `mux-apikey-helper` (`bin.install`), and update the formula's caveats
  to mention `mux init` / the sysop UI on `:8947`.
- **`.github/workflows/release.yml`:** mirror hadron's (it has one) — on tag
  push (`v*`), build all targets via `make package-release`, compute checksums,
  create a GitHub release, attach tarballs + checksums, and render/emit the
  Homebrew formula. Keep `ci.yml` (it stays the `make check` gate).
- Verify the frontend build is reproducible in CI (Node/pnpm step before the Go
  embed build).

#### Files

- `/Users/chrispian/dev/hollis-labs/apps/tether/scripts/release.sh` (edit)
- `/Users/chrispian/dev/hollis-labs/apps/tether/scripts/render-homebrew-formula.sh` (edit)
- `/Users/chrispian/dev/hollis-labs/apps/tether/.github/workflows/release.yml` (new)

#### Acceptance criteria

- [ ] `make package-release VERSION=…` produces tarballs that contain
      `tether_sysop` (verify by listing the archive).
- [ ] Rendered Homebrew formula installs all three binaries.
- [ ] `release.yml` validates (actionlint / dry-run) and is wired to `v*` tags.
- [ ] Frontend embed build runs in the release path (no "missing dist" errors).
- [ ] `make check` still green.

#### Scope fences

- Do not change the daemon/CLI runtime — packaging + CI only.
- Do not publish a real tag/release as part of this task (wiring + a dry-run /
  test tag is enough; the operator cuts the actual beta tag).

---

### T-v06x-01-11: PTY deprecation marker + ADR 0044 + docs + close

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [beta, adr, docs, deprecation]
**depends_on:** T-v06x-01-01, T-v06x-01-03, T-v06x-01-04, T-v06x-01-05, T-v06x-01-10

#### Problem

Two doc-of-record gaps + the install docs lag the new commands. PTY is declared
not-user-facing (D7) but carries no marker in code
(`internal/app/runtime_resolver.go:36-37`). And there's no ADR capturing the
beta onboarding decisions.

#### Fix direction

- **PTY marker:** add a deprecation comment + (if cheap) a `Deprecated:` doc
  comment / log-warn when the PTY runtime is resolved, so D7 is enforced in code,
  not just asserted. Do **not** remove PTY this sprint.
- **ADR `0044-beta-onboarding-and-install.md`** (next free number — 0041/0042/0043
  taken): record D1–D9, the CLI-only-wizard call, auto-seed + `mux init` split,
  detect+paste-no-Browse, Tools & Broker scope, and the deferred P1 list.
  Format per `docs/adr/0036-hardening-standards.md`.
- **`docs/install.md`:** replace the manual `install -d … && cp -R examples/…`
  First-Time-Setup steps with `mux init` (keep the manual path as a fallback
  appendix). Document `mux detect` / `mux doctor`.
- **README.md:** add `mux init` + the sysop UI (`:8947`, now shipped) to the
  quick-start.
- Update `beta-readiness.md` to check off the shipped items at sprint close.

#### Acceptance criteria

- [ ] PTY runtime carries a deprecation marker; resolving it still works (not
      removed) but is clearly marked.
- [ ] ADR 0044 lands, dated, sequential, format-compliant.
- [ ] `docs/install.md` leads with `mux init`; `mux doctor`/`mux detect`
      documented.
- [ ] README quick-start mentions `mux init` + the shipped UI.
- [ ] `make check` green.

#### Scope fences

- Do not remove the PTY runtime (D7 — marker only this sprint).
- No new ADRs beyond 0044.

---

## Deferred (P1+) — out of scope for this sprint

Tracked in `beta-readiness.md` §3-P1/P2 and §9-P1; do **not** pull into v06x-01:

- GUI setup wizard (`BlueprintWizardPage`-style) — CLI-only for beta (D2).
- Native Browse pickers / `/api/fs/browse` listing (D1).
- Embedded "known-templates" Add→pick→configure registry (D9).
- Subprocess + streaming session-output capture → store (PTY excluded entirely).
- Per-tool policy engine / deny-lists (D6 — coming-soon only).
- Errors rollup across the activity feed; structured (slog/JSON) daemon logging.
- Help page with copy-paste catalog snippets + "Create" buttons.

## Known limitations to record at close

- Per-tool policy / deny-lists not in beta (controls are server/scope-level).
- PTY session output not captured; PTY marked deprecated but not removed.
- Path validation is warn-don't-gate existence/exec only — a save can persist a
  not-yet-existing path by design.

---

## Sprint discipline (per CLAUDE.md)

- Branch off `main`; FF-merge at close; delete branch. No work on unrelated
  branches.
- `make check` green before every task-closing commit, no exceptions.
- Per-task subagent dispatch with `isolation: "worktree"` for parallel work in
  separate areas (T-01 is the shared foundation — land it first; T-05, T-10 are
  largely independent and can run in parallel once T-01 is in).
- Commit incrementally: `feat(onboarding): T-v06x-01-NN — <brief>`. End commit
  messages with:
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  ```
- Locked decisions (D1–D9) don't reopen. Surface a question before working
  around any of them.

---

## Dependency / ordering notes

```
T-01 (setup foundation: detect + embed/seed)  ← land FIRST
 ├─ T-02 (daemon auto-seed)
 ├─ T-03 (mux init)
 ├─ T-04 (mux detect / doctor)
 └─ T-06 (seed content finalization)
T-05 (daemon logs → file + tail)   — independent, parallel
T-07 (settings validation)         — independent (reuses setup detect)
T-08 (surface MCP/tool errors)     — independent, UI-only
T-09 (Tools & Broker view)         — independent
T-10 (release packaging + CI)      — independent, parallel
T-11 (PTY marker + ADR + docs)     — LAST (depends on the shipped surfaces)
```

---

## Hand-off snippet for the parallel agent

> You're executing Sprint v06x-01 — Beta Onboarding & Install Story for Tether.
> Goal: take a new user from `brew install` to a working, self-explaining install
> with no hand-edited YAML, and give operators enough log/error visibility to
> debug their own setup. Read `planning/docs/beta-readiness.md` (the audited
> assessment of record) and this sprint file end-to-end before starting.
> Decisions D1–D9 are LOCKED — wizard is CLI-only (`mux init`), detect+paste only
> (no Browse), daemon both auto-seeds AND `mux init` does guided setup, PTY is
> marked-deprecated-not-removed, Tools & Broker is read + light-edit only. Land
> T-01 (the `internal/setup` foundation) FIRST — every onboarding task consumes
> it. `make check` green is non-negotiable before any task-closing commit. ADR
> 0044 is the last task. Do not pull any "Deferred (P1+)" item into this sprint.
