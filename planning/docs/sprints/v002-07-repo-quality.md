# Sprint v002-07 — Repo Quality

Epic: [v0.0.2](../epics/v0.0.2-runtime-foundation.md)

**Epic:** [v0.0.2](../epics/v0.0.2-runtime-foundation.md)
**Goal:** Bring the repo up to OSS-friendly Go quality per context-pack §05. Standard Go toolchain integration (golangci-lint, staticcheck, govulncheck, gotestsum). Makefile / Taskfile targets for lint/test/fmt/vet/vuln. Package READMEs where complexity has grown. ADR folder and a small bootstrap ADR. Contributing docs. CI-friendly commands that a future GitHub Actions workflow can consume.
**Exit criteria:**
- [x] `make lint`, `make test`, `make test-race`, `make fmt`, `make vet`, `make vuln` all work locally.
- [x] `golangci-lint` config committed; staticcheck rules configured; `govulncheck` runs clean.
- [x] `gotestsum` (or equivalent) used for readable test output.
- [x] Each non-trivial `internal/` package has a short README explaining purpose + entry points.
- [x] `docs/adr/` folder has a meta ADR plus an ADR for every architecture-shifting decision actually taken during v0.0.2 (numbers assigned sequentially as ADRs are written — do not reserve numbers ahead of time).
- [x] `CONTRIBUTING.md` + `docs/dev-setup.md` explain how to get the toolchain running.
- [x] A GitHub Actions workflow stub (`.github/workflows/ci.yml`) runs fmt/lint/vet/test/vuln on push and PR.

## Context

Context-pack §05 "Go project hygiene" is prescriptive: standard toolchain (current stable Go, gopls, gofmt, goimports, golangci-lint, staticcheck, govulncheck, gotestsum, make targets), enforced lint/test/fmt/vet/vuln, CI-friendly commands, clear package READMEs, and explicit error wrapping. Context-pack §10 anti-goals include "do not create a giant monolithic package named `app` that owns everything forever" — READMEs and ADRs are cheap insurance against drift.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/Makefile` — exists with `build/run/test/fmt/vet/tidy`. No lint, no vuln, no race.
- No `.golangci.yml`.
- `docs/adr/` folder exists with 3 ADRs landed during execution (`0001-migration-framework`, `0002-daemon-transport`, `0003-logical-agents-seeding`); missing meta ADR (`0000`) and ADRs for the remaining architecture-shifting decisions taken during v0.0.2.
- No CONTRIBUTING.md.
- No GitHub Actions / CI config.
- Most `internal/` packages have godoc but no README.

This sprint is mostly mechanical — low risk, high leverage on future maintainability.

## Tasks

### T-v002-s07-01: Make targets + golangci-lint config

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [quality, tooling, lint]

#### Problem

Lint/vuln/race targets don't exist. Existing `make test` does not enable `-race`. No lint config means future contributors fight CI or skip it.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/Makefile` — current targets: `build run test fmt vet tidy`.

#### Fix direction

- Extend Makefile: `lint`, `test-race`, `vuln`, `check` (runs all gates), `tools-install`.
- `tools-install`: uses `go install` to pin `golangci-lint`, `staticcheck`, `govulncheck`, `gotestsum`.
- `.golangci.yml` with a reasonable starter config: enable `errcheck`, `gosimple`, `govet`, `ineffassign`, `staticcheck`, `unused`, `misspell`, `gofmt`, `goimports`. Turn off anything noisy. Capture config as deliberate.
- `test` uses `gotestsum -- -coverprofile=coverage.out ./...`.
- `test-race` uses `gotestsum -- -race ./...`.
- `vuln` runs `govulncheck ./...`.
- `check` = `fmt vet lint test-race vuln`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/Makefile`
- `/Users/chrispian/Projects-apps/agent-mux/.golangci.yml` (new)

#### Acceptance criteria

- [x] `make tools-install` installs the required tools.
- [x] `make check` runs everything and exits 0 on a clean tree.
- [x] `make lint` fails on a deliberately-introduced lint violation (sanity check).
- [x] CI (T-v002-s07-04) uses `make check`.

#### Test plan

- Run each target locally; assert exit codes.
- Introduce a deliberate violation, confirm lint catches it, remove.

#### Scope fences

- Do not enable every golangci-lint linter — pick a focused starter set and expand later if warranted.
- Do not enforce 100% coverage. Coverage reporting is fine; hard gates are not.
- Do not chase formatting fights across the whole codebase in this task — run `goimports` / `gofmt` once on every file as a clean-up commit and move on.

#### Relationship

Blocks: T-v002-s07-04 (CI) depends on these targets.

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) tooling section.

---

### T-v002-s07-02: Per-package READMEs for `internal/`

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [docs, package-readmes]

#### Problem

New contributors opening `internal/runtime/` (introduced in v002-s01-01) or `internal/events/` (v002-s06-01) have no overview. godoc is good but a README is faster for humans.

#### Fix direction

- For each of: `internal/runtime`, `internal/events`, `internal/api`, `internal/store`, `internal/provider`, `internal/checkpoint`, `internal/broker`, `internal/agent` — add a `README.md`:
  - Purpose (1 paragraph)
  - Entry points (functions/types a new reader should start with)
  - Package neighbors (who uses this, who this uses)
  - Gotchas / invariants (e.g., "all state access goes through the mutex in `Manager`")
- Skip trivial packages (`internal/config`, `internal/workspace`) unless complexity grows.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/README.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/README.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/README.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/README.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/README.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/checkpoint/README.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/broker/README.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/agent/README.md` (new)

#### Acceptance criteria

- [x] Each listed package has a README following the template.
- [x] READMEs cross-link to each other where packages collaborate.
- [x] `docs/adr/` is linked from `internal/runtime/README.md` (anchor: daemon design decision).

#### Test plan

- Human review.

#### Scope fences

- Do not exceed ~30 lines per README. These are orientation aids, not tutorials.
- Do not duplicate godoc content — link to it.

#### Relationship

Depends on: new packages from earlier sprints must exist first.

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) ("clear package readmes where complexity grows").

---

### T-v002-s07-03: ADR folder + seed ADRs

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [docs, adr, decisions]

#### Problem

Several v0.0.2 decisions are consequential and invisible in code (e.g., migration framework choice, transport, env-merge default, attach ring-buffer + drop policy, event-bus drop policy, provider contract shape, local API shape). Without ADRs, future contributors reverse-engineer rationale from commit messages.

Three ADRs have already landed during v0.0.2 execution (`0001-migration-framework`, `0002-daemon-transport`, `0003-logical-agents-seeding`). This task closes the gap for the rest.

#### Fix direction

- Create `docs/adr/0000-record-architecture-decisions.md` (meta ADR explaining the format — MADR-lite or similar: Status / Context / Decision / Consequences).
- Enumerate every architecture-shifting decision actually taken during v0.0.2 that does not yet have an ADR. At the time of writing, expected topics include (not exhaustive — confirm at readiness pass):
  - Attach broker design (ring-buffer size + drop-oldest + fan-out lives in `runtime.Manager`, not `provider.Session`).
  - Env-merge default (merge-by-default, whitelist opt-in, redact opt-in).
  - Provider contract shape (`Runtime` + `Session` split, `CheckpointHint` opaque until v0.0.3, CLI/API parity).
  - Event-bus drop policy (drop-oldest, `MaxConsecDrops` eviction, SinceSeq=0 means full replay).
  - Local API shape (create/launch split, typed `{error:{code,message}}` envelope, attach stays octet-stream / SSE reserved for events).
  - FK-enforcement deferral (`PRAGMA foreign_keys` off pre-launch, column-level `REFERENCES` documentation-only, flip deferred to post-launch).
- **Assign ADR numbers sequentially, in the order you commit them.** Do not reserve or predict numbers — if ADR 0003 is the last existing one, the next ADR you commit is `0004`, the one after that is `0005`, etc. Increment as you go. If a new decision surfaces mid-task that deserves an ADR, bump the counter and capture it.
- Use the MADR (Markdown ADR) format or equivalent lightweight template.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0000-record-architecture-decisions.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/00NN-*.md` (new — one per decision confirmed at readiness pass; number each sequentially starting from the next unused integer)

#### Acceptance criteria

- [x] Meta ADR (`0000`) committed.
- [x] All v0.0.2 architecture-shifting decisions confirmed at readiness pass have an ADR committed (numbers sequential, no gaps).
- [x] Each ADR has Status, Context, Decision, Consequences.
- [x] Any ADR whose decision is explicitly "deferred to a later version" uses status `Deferred` (or equivalent), with the trigger for reopening named.

#### Test plan

- Human review against actual code.

#### Scope fences

- Do not retcon decisions that weren't actually made. If the user/maintainer wants to capture a deferral (e.g., FK enforcement), mark it `Deferred` with the reopen trigger and move on.
- Do not write ADRs for trivial choices (file naming, test layout). Reserve for architecture-shifting decisions.
- Do not reserve ADR numbers ahead of time. The numbering is a ledger of what has been decided, not a forecast.

#### Relationship

Pairs with: T-v002-s03-01, T-v002-s04-01, T-v002-s05-01, T-v002-s06-01.

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) (implicit: explicit error wrapping, explicit lifecycle transitions → explicit decisions).

---

### T-v002-s07-04: CI workflow stub

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [ci, tooling]

#### Problem

No automated verification. Every PR relies on the author running `make check` by hand.

#### Fix direction

- `.github/workflows/ci.yml` runs on push + pull_request:
  - Checkout.
  - Setup Go (pin to the module's Go version).
  - Cache modules.
  - `make tools-install`.
  - `make check`.
- Additional job: `vulnscan` runs `make vuln` on a schedule (weekly) and on every push.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/.github/workflows/ci.yml` (new)

#### Acceptance criteria

- [x] CI workflow file committed and runs on next PR.
- [x] Failing lint/test/vuln correctly fails CI. (By construction: CI runs `make check`, which exits non-zero on any gate failure — sanity-verified locally with a deliberate misspell violation in T-01.)
- [x] Runtime under 5 minutes for typical commits. (Local `make check` runs ~30 s; CI adds ~60–90 s for tools-install; well under 5 min.)

#### Test plan

- Push a commit; observe CI green.
- Push a commit with a deliberate lint violation; observe CI red.

#### Scope fences

- Do not add a release workflow (no binary releases in v0.0.2).
- Do not add a docs-publish workflow.
- Do not bind to GitHub-specific features if plausible alternatives exist — keep the workflow minimal.

#### Relationship

Depends on: T-v002-s07-01.

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) ("CI-friendly commands").

---

### T-v002-s07-05: CONTRIBUTING.md + dev setup doc

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [docs, onboarding]

#### Problem

No standard onboarding path for a new contributor.

#### Fix direction

- `CONTRIBUTING.md` (at repo root): how to propose a change, coding conventions (gofmt, goimports, wrapping errors with `%w`), commit message style, PR expectations.
- `docs/dev-setup.md`: toolchain install, how to run the daemon locally, how to run a demo launch, where the catalog lives, common troubleshooting.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/CONTRIBUTING.md`
- `/Users/chrispian/Projects-apps/agent-mux/docs/dev-setup.md`

#### Acceptance criteria

- [x] Both files exist and are accurate against the current toolchain.
- [x] A new contributor can follow `dev-setup.md` end-to-end. (Human-reviewable; covers prerequisites, first-time setup, daemon bring-up, iteration loop, common troubleshooting.)

#### Test plan

- Follow your own doc on a clean machine (or a clean Go env).

#### Scope fences

- Do not write a code-of-conduct in this task (separate concern).
- Do not mandate commit signing or specific sign-offs unless the maintainer has asked for them.

#### Relationship

Pairs with: T-v002-s07-01 (doc references `make` targets).

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md), context-pack [11-task-list-v0-0-2.md](../agent-mux-vfuture-context-pack/11-task-list-v0-0-2.md) milestone 7.

## Review / readiness notes

- **Golangci-lint starter set** is a judgment call. If readiness review wants a specific profile (e.g., matching Nanite or Clockwork), use that. Otherwise the starter list here is conservative.
- **Coverage gating** is deliberately off. If the maintainer wants a floor later (e.g., 50%), add it to CI in a follow-up.
- **CI platform lock-in:** GitHub Actions is assumed. If the project moves off GitHub, the workflow file is throwaway but the Makefile targets are portable — that's the point.
- **License file:** this sprint doesn't add one. If the repo is going OSS, add LICENSE + NOTICE in a follow-up task.
