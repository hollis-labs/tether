# ADR 0036 — Hardening Standards

**Status:** Accepted
**Date:** 2026-05-12
**Supersedes:** —
**Superseded by:** —
**Related:** ADR 0002 (daemon transport), ADR 0010 (typed error envelope), Vanta `feedback_go_ecosystem_baseline`, Vanta `feedback_design_philosophy`, Vanta `feedback_service_invariants`

## Context

v005-10 was the first sprint where the deliverable is "the code is better" rather than "a new feature exists." Mux is approaching beta and needs a documented set of hardening standards so future audits don't re-derive them every time. The audit captured in `agent-workspaces/execution/agent-mux/v005-10-hardening-audit/2026-05-12/audit-findings.md` covered:

1. God objects / oversized files / oversized functions
2. Test coverage map (per-package and cross-package aggregate)
3. Error-handling patterns
4. Dead code
5. Comment debt + dependency hygiene
6. Security smells
7. Build / dev tooling
8. Portfolio-invariants drift check

The findings split into P0 (executed this sprint), P1 (sequenced into v005-10b), and P2 (capture-and-defer). This ADR records the standards Mux applies going forward so that the audit becomes recurring discipline rather than a one-shot exercise.

## Decision

### 1. God-object heuristics

A file, struct, or function is flagged for split when **any** of these holds:

- **File:** > 500 LOC **and** > 10 public functions/methods.
- **Struct:** > 12 fields.
- **Function:** > 80 LOC.

Heuristics are not absolute bans. Each flag earns a per-case verdict: **split / refactor / leave-with-reason**. Cohesive single-domain files (e.g., MCP tool registration clusters where every function is one handler for the same tool group) may legitimately exceed the file threshold; document the reason inline or in the audit findings.

Special invariant carried forward from prior ADRs: **`internal/app` stays thin.** Service struct beyond ~10 fields or `service.go` past ~300 LOC means the composition root has accreted domain logic that belongs in sibling files. v005-10 P0-A split `service.go` from 814 LOC across `catalog.go`, `session_lifecycle.go`, `session_io.go`, `codex.go`, `resume.go`, `runtime_helpers.go`, leaving 213 LOC.

### 2. Quality gate (`make check`)

`make check` runs the full pre-merge gate:

```
fmt → vet → lint → test-race → vuln → coverage-report
```

- **`fmt`** — `go fmt ./...` clean.
- **`vet`** — `go vet ./...` clean.
- **`lint`** — `golangci-lint run --max-issues-per-linter=0 --max-same-issues=0` clean. The `.golangci.yml` config mirrors the portfolio shared shape (see `feedback_go_ecosystem_baseline`); changes to the ruleset require an ADR amendment, not silent drift.
- **`test-race`** — `go test -race -coverpkg=./... -coverprofile=coverage.out ./...` clean. The `-coverpkg=./...` flag ensures cross-package tests credit the packages they actually exercise, not just the test binary's own.
- **`vuln`** — `govulncheck ./...` clean.
- **`coverage-report`** — Aggregate coverage printed at end of `make check`. **Not a hard gate this sprint** — the baseline (50.7% → 52.8%) is captured so v005-10b can pick a threshold. v005-10b ratchets the threshold upward each sprint.

CI runs `make check` on push/PR and weekly via scheduled cron (so newly-disclosed dependency vulnerabilities surface between pushes).

### 3. Error-handling conventions

- **Wrap with `%w` for chain-preserving errors.** No `%v` on error values. No `pkg/errors` legacy. 158 sites already comply; new code must too.
- **Sentinel errors are package-scoped.** Use `var ErrFoo = errors.New(...)` at the package level; export only when callers need to branch via `errors.Is`. No global error catalog.
- **`panic` allowed only in constructors / init for programmer-error invariants** (current count: 1 site in `internal/mcpadapter/events_store.go`, annotated). Never in request paths.
- **Silently dropped errors get a logged warning.** Future sites that need to swallow an error (event-bus publishes, write-error on a half-closed connection) wrap with `slog.Warn` or `slog.Debug` so operators can diagnose. The v005-10b sprint adds logging to the 4 + 7 sites identified by this audit; the rule applies to all new code.

### 4. Test coverage

- **Per-package coverage** is reported via `go test -cover ./...` and indicates **unit-test depth** for that package.
- **Aggregate cross-package coverage** is reported via `go test -coverpkg=./... -coverprofile=coverage.out ./...` and indicates **real coverage** — handlers in `internal/api` test paths through `internal/app`, the profiler credits both correctly.
- **Honest reporting:** `make check` reports the aggregate. Per-package depth is available via `make coverage` after `make test` for drill-down.
- **Zero-test packages need explicit justification.** Pure-type packages (`internal/agent/model.go`, `internal/checkpoint/model.go`, `internal/session/state.go`) are documented exceptions: the structs carry no behavior. Any new package with executable code needs at least a smoke test.

### 5. Dependency hygiene

- **`go mod tidy` clean** is a pre-commit check (folded into `make tidy` if/when added to `make check`).
- **Outdated dep policy:** patch + minor bumps are non-blocking; major bumps require changelog read + ADR if portfolio-impacting. v005-10b lands the next bulk refresh.
- **No hand-rolled HTTP provider clients.** When the API provider unparks, it MUST use official SDKs (`anthropic-sdk-go`, `openai-go`) behind the portfolio's `Provider` interface, per `feedback_go_ecosystem_baseline`.
- **MCP server library:** `mark3labs/mcp-go` (until the official MCP SDK matures).
- **No `pkg/errors`, no hand-rolled JSON-RPC MCP code.**

### 6. Dead code

- **`deadcode -test ./...`** is the canonical detector.
- Dead unexported helpers: delete on sight.
- Dead exported funcs: triage. Compat shims and removed-API stubs → delete (pre-launch repo, per `feedback_no_compat_shims`). Forward-looking scaffolding with passing tests → keep, document intent (see Option A in audit §4).

### 7. Security smells

- **`//nolint:gosec` annotations must carry a reason.** All current annotations (G115/G204/G304/G703) are well-documented.
- **File modes are uniform:** directories `0o750`, files `0o600` for state, executables only when needed.
- **No hardcoded paths.** Operator-controlled paths come from env vars (`MUX_*`), catalog YAML, or HOME-relative defaults.
- **Bounded reads at every external boundary.** Use `io.LimitReader` on HTTP bodies (audit fixed bootgen `http:` slot at 4 MiB), daemon responses, and any caller-provided payload. The `internal/api/input.go` cap pattern is the reference.
- **`exec.Command` sites are audited:** three exist today, all annotated with the trust boundary. v005-10b decides the Tier-2 boot-profile `cmd:` slot security model.

### 8. Documentation

- **ADR-worthy decisions go in `docs/adr/NNNN-topic.md`.** Sequential numbering; next free is currently `0037`. ADRs are short and decision-focused — context, decision, alternatives, consequences. Implementation detail belongs in code comments + this ADR's references.
- **Topic docs** in `docs/<area>.md` cover user-facing surfaces (`acp.md`, `mcp.md`, `sandboxing.md`, etc.).
- **Audit-findings docs** in `agent-workspaces/execution/agent-mux/<sprint>/<date>/audit-findings.md` are the source of truth for hardening punch lists. Sprint outlines reference them by section ID (e.g., "P1-A from audit §1").

### 9. Remediation roadmap

- **v005-10 (this sprint):** P0 items executed (8 / 8 shipped). Captured in commits `8c39049`, `0ca7026`, `8dcae58`, `dd09659`, `23e5a0e`, `d3084ab`, `2b50d6d`. ADR 0036 records standards.
- **v005-10b (next sprint):** P1 items captured in `agent-mux-v0-pack/docs/sprints/v005-10b-hardening-remediation-2.md`. Adds coverage hard gate, slog.Warn on event-bus publishes, bulk dep refresh, bootgen Tier-2 security decision, mcpadapter / client / cmd test seam uplifts.
- **v005-10c and beyond:** P2 items re-triage at sprint open. Any P2 promoted to P1 by changing circumstances re-enters the sequence.

### 10. Future audits

Hardening audits are recurring discipline, not a one-shot. Cadence: at minimum, every major release. Specifically:

- **Pre-beta** (now): full audit (v005-10).
- **Pre-1.0:** full audit + drift check vs portfolio invariants.
- **Post-incident:** narrow audit on the area that broke.
- **Annual:** dependency-bump-driven full audit.

Each audit produces an `audit-findings.md` in the tracking root, captures decisions in a new ADR, and either executes P0 inline or schedules a remediation sprint.

## Alternatives considered

- **Hard coverage gate this sprint.** Rejected: floor wasn't known; picking a number blind risks blocking on coverage work that wasn't scoped. v005-10b lands the threshold once the floor is captured.
- **Aggressive god-object split via type extraction.** Considered: extract `sessionService`, `codexThreadCache`, `runtimeRegistry` as separate types held as fields on Service. Rejected for this sprint: Service struct's 8 fields is under the threshold; adding new types to hold what's already a domain method cluster would add indirection without reducing concept count. File-split addresses the actual LOC violation; type-split deferred unless future audits flag struct-shape issues.
- **Renovate / Dependabot now.** Deferred: low signal currently (20 outdated deps mostly patch/minor; portfolio-lib refresh is the bigger ticket). v005-10b T-19.
- **Delete middleware scaffolding (ADR 0021 Phase 3).** Rejected: `internal/mcpadapter/middleware*.go` has 174 LOC of tests pinning the design; deleting working scaffolding to rebuild later violates the spirit of `feedback_no_compat_shims` (which targets legacy aliases, not forward-looking scaffolding). Added package-level note (v005-10b T-21) so future audits don't re-discover.

## Consequences

- New code must clear `make check` including the aggregate coverage report. v005-10b's hard-gate threshold becomes a merge prerequisite.
- The `internal/app` thin-composition-root invariant is now codified, not just folklore in the boot prompt.
- Audit cadence is documented; future agents who open the sprint sequence know to schedule periodic audits.
- The remediation roadmap (P0 here, P1 in v005-10b, P2 capture-and-defer) sets a default for how hardening-class findings flow across sprints.
