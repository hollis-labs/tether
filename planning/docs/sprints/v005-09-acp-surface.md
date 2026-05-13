# Sprint v005-09 — ACP Surface

**Epic:** Mux beta-readiness push
**Status:** staged (depends on v005-08)
**Branch:** `feat/v005-09-acp-surface`
**Dependency:** v005-08 (Agent Ops) merged to `main`
**Date:** 2026-05-11

---

## Goal

Add Agent Client Protocol (ACP) as a fourth consumer surface alongside CLI / MCP / HTTP. `mux acp` subcommand mirrors `mux mcp`, exposing the service layer over ACP's JSON-RPC framing for editor consumption (Zed, JetBrains, Avante.nvim, CodeCompanion.nvim).

**Expose only, not consume.** Mux speaks ACP outbound to editors. Consuming external ACP agents is post-beta plugin-sdk territory.

---

## Sub-boot-prompt

`~/Projects-apps/agent-workspaces/boot/agent-mux/boot-prompt-v005-09-acp-surface.md` — finalize during readiness pass.

---

## Tasks (provisional — refine at boot)

- [ ] **T-v005-s09-01** — Read ACP spec end to end at `https://agentclientprotocol.com`. Map each ACP method to a Mux service-layer call. Capture in `tracking_root/acp-method-mapping.md`.
- [ ] **T-v005-s09-02** — `internal/acpadapter/` package mirroring `internal/mcpadapter/`. JSON-RPC framing, method dispatch, auth middleware.
- [ ] **T-v005-s09-03** — Service-layer routing for required ACP methods (session create/launch/send/stop/list).
- [ ] **T-v005-s09-04** — `cmd/mux acp` subcommand. Multi-client attach via existing fanout.
- [ ] **T-v005-s09-05** — Editor integration docs (Zed, JetBrains `acp.json`, Neovim plugin pointers).
- [ ] **T-v005-s09-06** — ADR 0034 — ACP surface adoption.
- [ ] **T-v005-s09-07** — Tests + smoke + `make check` + `make install` + FF-merge.

---

## Acceptance

- [ ] `mux acp` subcommand works
- [ ] Zed (or equivalent ACP editor) drives a Mux session end-to-end
- [ ] JetBrains `acp.json` example verified manually
- [ ] Multi-client attach works
- [ ] Auth + scope gating enforced (mirrors MCP)
- [ ] Editor integration docs land
- [ ] ADR 0034 committed
- [ ] `make check` green; `make install`
- [ ] FF-merged; branch deleted; parent boot prompt updated

---

## Out of scope

- Consuming external ACP agents (post-beta plugin)
- GUI integration with ACP
- ACP-specific provider features not mapping to existing primitives

---

## Decisions to confirm at boot

1. Method coverage subset for MVP (depends on spec read)
2. ACP "thread"/"session" → Mux logical-agent + session mapping
3. Multi-client semantics consistency with MCP

---

## Notes

- ACP's JSON-RPC framing parallels Codex `app-server`. Reuse primitives only if it's clean; don't force-share.
- Editor integration docs are user-facing; quality matters for beta launch.
