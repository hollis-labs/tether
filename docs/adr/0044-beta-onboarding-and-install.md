# ADR 0044 — Beta Onboarding & Install Story

**Status:** Accepted
**Date:** 2026-06-05
**Supersedes:** —
**Superseded by:** —
**Related:** ADR 0036 (hardening standards), `planning/docs/sprints/v06x-01-beta-onboarding.md`, `planning/docs/beta-readiness.md`

## Context

Tether's runtime was beta-solid but the install and first-run experience was not:
a user after `brew install` hit a manual cliff — hand-create `~/.tether/catalog/`,
copy example YAMLs, know which provider paths to fill in. Four P0 gaps blocked a
credible beta:

1. No first-run bootstrap (`mux init` did not exist).
2. sysop UI binary not in release tarballs or Homebrew formula.
3. Path/command fields freeform and unvalidated — typos silently accepted.
4. Auto-detection plumbing present in the adapter layer but never surfaced.

Sprint v06x-01 closed all four gaps plus two high-value observability gaps (daemon
logs, MCP/tool error surfacing). Nine decisions were locked during the assessment
phase; this ADR records them for future implementers.

## Decision

### D1 — File browse: detect + paste only for beta

sysop is a browser-served SPA (not Wails or Electron), so native file-picker
dialogs are not free. Beta ships auto-detect + manual paste; no Browse button, no
daemon-side `/api/fs/browse`. Browse is explicitly post-beta.

*Rationale:* don't block beta on a desktop-shell or new filesystem-listing
decision.

### D2 — Setup wizard is CLI-only (`mux init`)

No GUI wizard for beta. The GUI receives the settings-validation pass (T-07) but
the guided first-run flow lives entirely in the CLI.

*Rationale:* fastest to ship, works headless / over SSH, no GUI dependency.

### D3 — Catalog bootstrap = both auto-seed + `mux init`

The daemon self-seeds a minimal working catalog if none exists so it never
hard-fails on a missing `~/.tether/catalog/global.yaml`. `mux init` performs the
full guided setup (detect → edit/paste → skip-with-"set later" → write full
example catalog). Both consume the same embedded catalog source.

### D4 — "Set later" is a first-class affordance

Every non-required `mux init` step gets an explicit skip that prints where to
configure it later (Settings → Providers, etc.). Skipping never blocks completion.

### D5 — Validation is existence + executability only; save is not gated

Path fields check that the path exists on disk. The `command` field additionally
checks the exec bit. No deep semantic validation, no binary execution.
Validation is daemon-side (the daemon owns the filesystem the binaries live on)
via `POST /api/fs/validate`. Save is **not** blocked on a red status — a user may
legitimately configure a path that does not exist yet (warn-don't-gate).

### D6 — Tools & Broker scope for beta

Editable in beta: MCP per-server enable/disable, MCP visibility allowlists
(`mcp.servers[]`), scope grants (`scopes[]`). Shown read-only: AI `allow_tools`
(already editable on the AI page). Labeled "coming soon": per-tool enable/deny and
deny-lists. No per-tool policy engine ships in beta.

### D7 — PTY is not a user-facing runtime

subprocess and streaming(-stdio / jsonrpc-stdio) are the primary runtimes.
`internal/app/runtime_resolver.go` adds a deprecation comment + log-warn when the
PTY binding is resolved. PTY is **not** removed this sprint — marker-only until
output-capture is complete. PTY output capture is explicitly out of scope for beta.

### D8 — No raw-content leakage in seeds

Seeded example provider/MCP YAMLs carry empty `command` (adapter auto-detect) and
no secrets/tokens in `env`. The same importer-scope discipline as v060-02 applies:
seeds are identity/structure only.

### D9 — Embedded known-templates registry is post-beta

The §7.2 "Add → pick from known list → prefilled form" direction is P1. Beta seeds
the catalog YAMLs so first-run pages are non-empty; the Add flow stays as-is
(blank form + ID-prefix brand inference in `provider_runtime.go`).

## Shipped surfaces (sprint v06x-01)

| Surface | ADR clause | Key file(s) |
|---|---|---|
| `mux init` guided setup | D2, D3, D4 | `internal/setup/`, `cmd/mux/init.go` |
| Daemon auto-seed | D3 | `internal/app/service.go` |
| `mux detect` / `mux doctor` | — | `cmd/mux/detect.go`, `cmd/mux/doctor.go` |
| Daemon logs → `~/.tether/logs/muxd.log` | — | `cmd/mux/daemon.go`, `internal/api/logs*.go` |
| sysop Daemon Logs tail view | — | `apps/sysop/frontend/src/pages/logs.tsx` |
| `POST /api/fs/validate`, `GET /api/fs/detect` | D5 | `internal/api/fs.go` |
| Settings validation UI | D5 | `apps/sysop/frontend/src/pages/settings.tsx` |
| MCP server error surfacing | D6 | `apps/sysop/cmd/tether_sysop/main.go`, `pages/mcp.tsx` |
| Tool error text in Activity | — | `apps/sysop/frontend/src/pages/activity.tsx` |
| Tools & Broker page | D6 | `apps/sysop/frontend/src/pages/tools.tsx` |
| sysop in release tarballs + Homebrew | — | `scripts/release.sh`, `scripts/render-homebrew-formula.sh` |
| Tag-triggered release workflow | — | `.github/workflows/release.yml` |
| PTY deprecation marker | D7 | `internal/app/runtime_resolver.go` |

## Deferred (P1+)

- GUI setup wizard (D2 — CLI-only for beta).
- Native Browse pickers / `/api/fs/browse` (D1).
- Embedded "known-templates" Add→pick→configure registry (D9).
- Subprocess + streaming session-output capture → store (PTY excluded).
- Per-tool policy engine / deny-lists (D6 — "coming soon" label only).
- Errors rollup across activity feed; structured (slog/JSON) daemon logging.

## Known limitations

- Per-tool policy / deny-lists not in beta; controls are server/scope-level.
- PTY session output not captured; PTY is marked deprecated, not removed.
- Path validation is warn-don't-gate existence/exec only — a save can persist a
  not-yet-existing path by design (D5).

## Alternatives considered

- **GUI wizard (BlueprintWizardPage-style).** Rejected for beta: more complex,
  requires Wails or a bespoke multi-step flow, doesn't work headless. `mux init`
  handles the same job in the terminal. Wizard remains a tracked P1.
- **Native Browse picker via daemon-side `/api/fs/browse`.** Rejected: sysop is a
  plain HTTP SPA — native pickers require Wails or a security-sensitive fs-listing
  endpoint. Detect + paste covers the beta use case without new attack surface.
- **Gate save on red validation.** Rejected (D5): legitimate workflows involve
  provisioning paths that don't exist at config time. Warn-don't-gate is the safe
  default; a hard gate would regress real operator workflows.

## Consequences

- First-run setup is guided, idempotent, and works headless from a single command.
- The PTY runtime still resolves but logs a deprecation warning; future sprints
  can remove it once streaming-stdio + subprocess cover all use cases.
- Tools & Broker is a single view for scattered tool knobs; the per-tool policy
  engine is the documented next step for that page.
- Release artifacts are complete (sysop UI included) and reproducible (tag-
  triggered CI); formula rendering is automated.
