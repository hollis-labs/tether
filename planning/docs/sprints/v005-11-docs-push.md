# Sprint v005-11 — Docs Push (Beta-Ready)

**Epic:** Mux beta-readiness push (final sprint)
**Status:** staged
**Branch:** `feat/v005-11-docs-push`
**Dependencies:** v005-05 through v005-10 all SHIPPED to `main` (tip `2ba0481`)
**Date:** 2026-05-12

---

## Goal

Bring the docs set up to current state so beta consumers can use Mux end to end. Final sprint of the beta push. Zero code changes (modulo deleting orphaned files surfaced during audit). After this: Mux is beta-ready.

Three buckets:
1. **Refresh existing docs** that pre-date v005-05 — long-lived sessions are the default; TUI is gone; ACP is live; Agent Ops two-tier config; new HTTP endpoints; MCP-as-daemon-client routing.
2. **Write new docs** previous sprints surfaced but didn't deliver — `docs/long-lived-sessions.md`, `docs/architecture.md`, `docs/adr/README.md` (ADR index), `knowledge/projects/agent-mux.md` (closes the GAP flagged since v0.0.2).
3. **Audit + housekeeping** — per-package READMEs, top-level README, CHANGELOG backfill, LICENSE decision, parent boot prompt refresh.

---

## Sub-boot-prompt

`~/Projects-apps/agent-workspaces/boot/agent-mux/boot-prompt-v005-11-docs-push.md` — full detail, decisions, phases, acceptance.

---

## Tasks

- [ ] **T-v005-s11-01** — Audit: walk every `*.md` in repo + ADRs. Build a tracking-root spreadsheet of file → current state → action. Surface Decisions 1–4 (LICENSE, knowledge KB write-now, ADR index, CHANGELOG backfill) if unclear.
- [ ] **T-v005-s11-02** — Refresh existing docs: top-level `README.md`, `docs/api/README.md`, `docs/mcp.md`, `docs/dev-setup.md`, `docs/sandboxing.md`. Verify currency of `docs/acp.md`, `docs/agent-config-reference.md`, `docs/caller-launched-sessions.md`, `docs/skill-format.md`.
- [ ] **T-v005-s11-03** — Per-package READMEs audit (8 files: `internal/{checkpoint,broker,provider,agent,api,events,store}/README.md` + `pkg/claudestream/README.md`). Refresh, delete orphans, or no-op each.
- [ ] **T-v005-s11-04** — New docs: write `docs/long-lived-sessions.md` (conceptual), `docs/architecture.md` (high-level), `docs/adr/README.md` (ADR index covering 0000–0036).
- [ ] **T-v005-s11-05** — Cross-project KB: write `agent-workspaces/knowledge/projects/agent-mux.md` (closes long-standing GAP).
- [ ] **T-v005-s11-06** — `CHANGELOG.md` backfill (Keep-a-Changelog format) — v005-05 → v005-10 entries with ADR + commit references.
- [ ] **T-v005-s11-07** — Parent boot prompt refresh: `Where We Are` (anchored at 2026-04-27, five sprints stale), `Backlog` sweep, `Active Migration` table.
- [ ] **T-v005-s11-08** — LICENSE decision per Decision 1: ARR preview / PolyForm / BUSL / other. Surface to user. Capture decision in Vanta. Write LICENSE file if applicable.
- [ ] **T-v005-s11-09** — `make check` green. `make install`. FF-merge. Update tracking + Vanta (`mux_beta_push_roadmap_may_2026` → "BETA READY"). Sweep guard: zero `*.md` recommends `mux tui`.

---

## Acceptance

- [ ] Top-level `README.md` reflects current state
- [ ] `CHANGELOG.md` exists with v005-05 → v005-10 entries
- [ ] `docs/long-lived-sessions.md`, `docs/architecture.md`, `docs/adr/README.md` written
- [ ] `knowledge/projects/agent-mux.md` written (closes the GAP)
- [ ] All refreshed docs cross-linked and current
- [ ] Per-package READMEs audited; orphans deleted
- [ ] Parent boot prompt's `Where We Are` + `Backlog` + `Active Migration` refreshed
- [ ] LICENSE decision recorded in Vanta + LICENSE file updated if applicable
- [ ] No `*.md` recommends `mux tui`
- [ ] `make check` green; `make install`
- [ ] FF-merged; branch deleted; tracking root updated
- [ ] Vanta: `mux_beta_push_roadmap_may_2026` superseded to "BETA READY"

---

## Decisions to surface at boot

1. **LICENSE** — ARR preview / PolyForm Noncommercial / BUSL 1.1 / other?
2. **`knowledge/projects/agent-mux.md`** — write-now (rec) or defer?
3. **`docs/adr/README.md`** index — write (rec) or skip?
4. **CHANGELOG backfill** — backfill v005-05 → v005-10 (rec) or forward-only?

---

## Out of scope

- Code changes (except deleting orphaned files)
- Marketing / website / external content
- Video / GIF / screenshots
- New examples beyond `examples/`
- Backporting docs to older Mux versions
- Translations
- Per-ADR rewrites

---

## Notes

- Last sprint before beta. Quality > speed.
- Code examples must be runnable. Copy-paste-and-go.
- Don't oversell beta status — honest framing.
- Parent boot prompt should strip to a `docs/architecture.md` pointer + Vanta backlog reference, not duplicate state.
- After this: post-beta backlog includes v005-10b P1 cleanup + the live Vanta followups.

---

## After this sprint

Mux beta-ready. Parallel tracks resume (Mux native GUI MVP, Clockwork lib-tier adoption). Lib-tier v0.10.0 candidate: flip `AutoPlantBootDir` default once all three consumers adopt.
