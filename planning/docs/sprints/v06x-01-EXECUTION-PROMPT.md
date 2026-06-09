# Execution kickoff — Sprint v06x-01 (Beta Onboarding & Install Story)

> Paste the block below into a **fresh Claude Code session at the repo root**
> (`/Users/chrispian/dev/hollis-labs/apps/tether`). This session orchestrates;
> that session executes. Keep this file as the canonical hand-off.

---

You are the implementer for **Sprint v06x-01 — Beta Onboarding & Install Story**
in the Tether repo (`~/.tether` control plane; `mux` CLI + `muxd` daemon + sysop
GUI). Your job is to take a new user from `brew install` to a working,
self-explaining install with no hand-edited YAML, and give operators enough
log/error visibility to debug their own setup.

## Read first (in order, end-to-end, before any code)

1. `planning/docs/sprints/v06x-01-beta-onboarding.md` — your spec. Tasks
   T-v06x-01-01 … -11, with Problem / Fix direction / Files / Acceptance / Scope
   fences each. The **Exit criteria** and **Decisions locked (D1–D9)** sections
   are binding.
2. `planning/docs/beta-readiness.md` — the audited assessment of record (read the
   verification banner at the top; claims were re-checked 2026-06-05).
3. `CLAUDE.md` + `AGENTS.md` — project orientation, state roots, sprint discipline.
4. Skim `planning/docs/sprints/v060-01-registry-foundation.md` for the house
   style of a sprint (task shape, scope-fence discipline) — yours mirrors it.

## Locked decisions — do NOT reopen (full text in the sprint file, D1–D9)

- **D2:** Setup wizard is **CLI-only** (`mux init`). No GUI wizard this beta.
- **D1:** Path config is **detect + paste only**. No Browse button, no
  `/api/fs/browse` listing endpoint.
- **D3:** Catalog bootstrap is **both** — daemon **auto-seeds** a minimal catalog
  when none exists (never hard-fail), **and** `mux init` does full guided setup.
  Both consume the same `internal/setup` embedded catalog.
- **D5:** Settings validation is **existence + executability only**, and
  **warn-don't-gate** (a red field still saves).
- **D6:** Tools & Broker view is **read + light edit** — MCP enable/disable +
  visibility allowlists + scope grants editable; AI `allow_tools` read-only;
  per-tool policy is "coming soon" (no policy engine).
- **D7:** PTY is **marked deprecated, not removed**. subprocess + streaming are
  the primary runtimes.
- **D8/D9:** Seeds carry no secrets and empty `command` (rely on auto-detect);
  the embedded "known-templates" picker registry is post-beta.

If you believe a locked decision is wrong, **stop and surface it** to the
orchestrator — do not work around it silently.

## Execution order & method

- **Land T-v06x-01-01 FIRST.** It builds `internal/setup/` (detection helper +
  `//go:embed` starter catalog + `WriteCatalog`). Tasks 02, 03, 04, 06 all depend
  on it. Do not stub it — the consumers reuse the real adapter `Detect()`
  plumbing (`internal/provider/cli/*/cliadapter.go`); don't duplicate lookup logic.
- After T-01 is in and `make check` is green, the following can proceed in
  parallel (use `isolation: "worktree"` per CLAUDE.md sprint discipline when
  dispatching to sub-agents for areas that don't overlap):
  - T-02 (daemon auto-seed), T-03 (`mux init`), T-04 (`mux detect`/`doctor`),
    T-06 (seed content) — all touch onboarding but separate files.
  - T-05 (daemon logs → file + sysop tail), T-07 (settings validation),
    T-08 (surface MCP/tool errors), T-09 (Tools & Broker view),
    T-10 (release packaging + `release.yml`) — independent.
- **T-11 LAST** (PTY marker + ADR 0044 + docs + check off `beta-readiness.md`).
  It depends on the shipped surfaces.

## Discipline (non-negotiable)

- **Branch off `main`** (e.g. `feature/v06x-01-beta-onboarding`); FF-merge at
  close; delete the branch. No work on unrelated branches.
- **`make check` green before every task-closing commit** — `fmt + vet + lint +
  test-race + vuln + coverage-report`. No exceptions.
- **Commit incrementally:** `feat(onboarding): T-v06x-01-NN — <brief>`. End each
  commit message with:
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  ```
- **Check off acceptance boxes** in the sprint file as you complete each task.
- **Stay inside the scope fences.** Every task lists what NOT to touch. The
  "Deferred (P1+)" section names work that is explicitly out of scope — do not
  pull any of it in (no GUI wizard, no Browse, no session-output capture, no
  per-tool policy engine, no errors-rollup, no slog migration).

## Reporting back to the orchestrator

After each task lands (green + committed), post a short status: task ID, commit
SHA(s), which acceptance boxes are ticked, and any discovery that affects a later
task or bumps against a scope fence / locked decision. Batch nothing that needs a
decision — surface it immediately.

Start by confirming you've read the three docs, then create the branch and begin
T-v06x-01-01.
