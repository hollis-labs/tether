# Sprint v005-07 — Lib-Tier Adoption (v0.9.x)

**Epic:** Mux beta-readiness push
**Status:** staged (depends on v005-06)
**Branch:** `feat/v005-07-lib-tier-adoption`
**Dependencies:**
- v005-05 long-lived integration merged to `main` (SHIPPED — main tip `3927b82`)
- v005-06 TUI removal merged to `main` (precedes for clean tree)
- go-agent-sessions v0.9.2+ shipped (v0.9.0 BootDirSpec absorption + v0.9.1 lastSessionID pre-seed fix + v0.9.2 abnormal-wait diagnostic)

**Date:** 2026-05-11

---

## Goal

Adopt the v0.9.x lib substrate end-to-end. Bump `go.mod`, flip `StartOptions.AutoPlantBootDir=true`, delete Mux's remaining per-app planter code, wire `OnBootDirPlanted` to the event bus, and implement Codex `SendTurn` via the new `Manager.JsonRpcCall` capability-dispatch path. Smoke proves Claude (no regression) + Codex (new) work end-to-end across CLI/MCP/HTTP.

After this sprint: zero per-app planting code in Mux, Codex SendTurn functional, clean substrate for v005-08 Agent Ops to build on.

---

## Sub-boot-prompt

`~/Projects-apps/agent-workspaces/boot/agent-mux/boot-prompt-v005-07-lib-tier-adoption.md` — concrete API surface, decisions, phases, acceptance.

---

## Tasks

- [ ] **T-v005-s07-01** — Bump `go.mod` to `go-agent-sessions v0.9.2+`. `go mod tidy`. Confirm `go build ./...` + existing tests green.
- [ ] **T-v005-s07-02** — Flip `StartOptions.AutoPlantBootDir=true` at every `NewFromAdapter` call site in `internal/app/service.go`. Run existing tests; investigate any regressions.
- [ ] **T-v005-s07-03** — Delete `internal/provider/cli/claudestream/plantBootDir` + any remaining per-app planting helpers. Grep `plantBootDir` / `MkdirTemp.*boot` in `internal/ cmd/` returns zero matches. If `claudestream` package is empty post-delete, `git rm -r` it. Re-home `PlanScopedAdapter` if still useful.
- [ ] **T-v005-s07-04** — Define `BootDirPlanted` typed event. Wire `OnBootDirPlanted` callback at service-layer to emit the event through Mux's event bus. Test: subscriber sees event on session start.
- [ ] **T-v005-s07-05** — Implement Codex `SendTurn` via `Manager.JsonRpcCall`: lazy initialize + thread/start (cache thread_id in `SessionMeta["codex_thread_id"]`) + turn/start. Map errors (`ErrSessionNotJsonRpcCapable`, `ErrSessionNotRunning`, `*JsonRpcError`).
- [ ] **T-v005-s07-06** — Add one-line code comment near session-id store persistence call noting the cross-provider key collision risk; reference Vanta `followup_mux_session_id_store_namespacing`. Don't fix the bug here; just flag it.
- [ ] **T-v005-s07-07** — Live smoke: Claude streaming-stdio (KV-cache hit on turn 2 confirmed) + Codex app-server (same thread_id across 2 turns), each verified via CLI / MCP / HTTP. Record results in tracking root.
- [ ] **T-v005-s07-08** — ADR 0032 — Lib-Tier v0.9.x Adoption + Codex SendTurn. Update parent boot prompt's "Where We Are" + Active sub-boot-prompt pointer. `make check` green. `make install`. FF-merge; delete branch.

---

## Acceptance

- [ ] `go.mod` pins `go-agent-sessions v0.9.2+`
- [ ] `AutoPlantBootDir=true` everywhere
- [ ] `BootDirPlanted` event emitted
- [ ] Zero matches for `plantBootDir` / `MkdirTemp.*boot` in `internal/ cmd/`
- [ ] Codex `SendTurn` functional end-to-end with lazy init/thread/turn sequencing
- [ ] All error modes mapped correctly
- [ ] Tests pass under `-race`
- [ ] Smoke: Claude KV-cache hit on turn 2 (no regression)
- [ ] Smoke: Codex same thread_id across turns, verified CLI + MCP + HTTP
- [ ] ADR 0032 committed
- [ ] `make check` green; `make install`
- [ ] FF-merged; branch deleted; parent boot prompt updated

---

## Out of scope

- Agent Ops schema, discovery, skills, caller-provided launches (v005-08)
- MCP per-boot-profile wiring (v005-08)
- ACP surface (v005-09)
- Hardening (v005-10)
- Docs (v005-11)
- Session-id store namespacing (separate followup, flagged in code but not fixed)
- `mux mcp` daemon proxy (separate followup)

---

## Notes

- Focused mechanical sprint. Resist scope creep.
- v0.9.2's abnormal-wait diagnostic logs help debug any surprise exits during the live smoke.
- The `claudestream` package name is anachronistic; if anything remains, give it an honest name.
