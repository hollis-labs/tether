# Sprint v005-10 — Hardening Audit + Remediation Sprint 1

**Epic:** Mux beta-readiness push
**Status:** staged (depends on v005-09)
**Branch:** `feat/v005-10-hardening-audit`
**Dependency:** v005-09 (ACP surface) merged to `main`
**Date:** 2026-05-11

---

## Goal

Audit-driven hardening before beta. Two halves in one sprint:

**Half A — Audit.** Read the Mux codebase end to end. Produce a priority-tagged punch list covering god objects, test coverage gaps, error-handling inconsistencies, dead code, comment debt, security smells, dependency hygiene, and build/dev tooling. Captured in `tracking_root/audit-findings.md`.

**Half B — Remediation Sprint 1.** Execute P0 items. Capture P1+ as v005-10b/c/... follow-up sprints.

Goal: code shape reflects portfolio standards (`feedback_design_philosophy`, `feedback_go_ecosystem_baseline`, `feedback_service_invariants`) before beta launch. v005-10 (docs push) follows once hardening lands.

---

## Sub-boot-prompt

`~/Projects-apps/agent-workspaces/boot/agent-mux/boot-prompt-v005-10-hardening-audit.md` — full detail, decisions, audit checklist.

---

## Tasks (provisional — refine at boot)

- [ ] **T-v005-s10-01** — Walk `internal/*` + `cmd/*`. Flag god objects (file > 500 LOC, struct > 12 fields, func > 80 LOC). Annotate each: split / refactor / leave-with-reason.
- [ ] **T-v005-s10-02** — Coverage map: `go test -cover -coverprofile=cover.out ./...`. Catalog packages by coverage tier; annotate critical gaps with priority test types.
- [ ] **T-v005-s10-03** — Error-handling patterns audit: panic in non-init, swallowed errors, wrapping inconsistencies, error-as-data vs error-as-control-flow.
- [ ] **T-v005-s10-04** — Dead code, comment debt (TODO/FIXME/XXX/HACK — cross-reference Vanta follow-ups), dependency hygiene (`go list -m -u all`).
- [ ] **T-v005-s10-05** — Security smells: hardcoded paths/URLs, shelled commands, file mode bits, unbounded reads.
- [ ] **T-v005-s10-06** — Build/dev tooling: Makefile, CI workflows, dev-setup docs.
- [ ] **T-v005-s10-07** — Portfolio invariants drift check (Vanta `feedback_service_invariants`, `feedback_design_philosophy`, `feedback_go_ecosystem_baseline`).
- [ ] **T-v005-s10-08** — Capture `tracking_root/audit-findings.md` with P0/P1/P2 priorities. Surface to user before remediation.
- [ ] **T-v005-s10-09** — Execute P0 items.
- [ ] **T-v005-s10-10** — Add `golangci-lint` to `make check` with portfolio-baseline config. Add `go test -cover` reporting to `make check` (no hard gate this sprint).
- [ ] **T-v005-s10-11** — Capture P1+ items as v005-10b/c... future-sprint outlines.
- [ ] **T-v005-s10-12** — ADR 0035 — Hardening Standards. `make check` green. `make install`. FF-merge.

---

## Acceptance

- [ ] Audit findings produced, priority-tagged
- [ ] P0 items executed
- [ ] P1+ items captured as future-sprint outlines
- [ ] `golangci-lint` + `go test -cover` reporting in `make check`
- [ ] ADR 0035 committed
- [ ] `make check` green
- [ ] FF-merged; branch deleted; parent boot prompt updated

---

## Decisions to confirm at boot

1. Audit scope — full repo (rec) vs internal/* only
2. Coverage gate threshold — baseline first this sprint, gate in v005-10b
3. Lint config — golangci-lint portfolio-baseline (rec)
4. God-object heuristics — file > 500 LOC + > 10 public funcs OR struct > 12 fields (rec; judgment per case)
5. Build tooling overhaul — defer non-critical to v005-10b

---

## Out of scope

- Build/CI overhaul beyond what audit P0 flags
- Docs push (v005-10)
- New features
- Refactoring for elegance beyond what audit flags

---

## Notes

- First sprint where deliverable is "code is better" not "feature exists." Resist scope creep.
- v005-11 (docs push) follows. Hardening must land first so docs reflect the final shape.
