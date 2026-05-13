# Sprint v005-08 — Agent Ops (Two-Tier Config)

**Epic:** Mux beta-readiness push
**Status:** staged (depends on v005-07)
**Branch:** `feat/v005-08-agent-ops`
**Dependencies:**
- v005-06 TUI removal merged to `main`
- v005-07 lib-tier adoption merged to `main` (AutoPlantBootDir=true, claudestream planter deleted, Codex SendTurn functional)

**Date:** 2026-05-11

---

## Goal

Implement Mux's two-tier agent configuration model.

- **Tier 1 (Mux-owned internal agents):** the agents/skills/prompts Mux uses for its own AI features. Mux owns the YAML, manages via CLI.
- **Tier 2 (Caller-provided launches):** when Nanite, Clockwork, or external consumers use Mux to launch sessions, they pass agent definitions + system prompts + skills as file pointers (or inline payloads via MCP/HTTP). Mux routes to BootDirSpec; doesn't store the content.

MCP server allowlist lives on the boot profile, not the agent. Three-layer precedence: project > user > system. Skills compile per provider (Claude + Codex in scope). Per-launch override JSON flag. Builds on go-agent-sessions v0.9.0's BootDirSpec absorption.

---

## Sub-boot-prompt

`~/Projects-apps/agent-workspaces/boot/agent-mux/boot-prompt-v005-08-agent-ops.md` — full detail, decisions, acceptance.

---

## Tasks

- [ ] **T-v005-s08-01** — Schema extensions: agent YAML gains `system_prompt`, `agent_prompt`, `skills` (populated), `provider_overrides`. Boot profile YAML gains `mcp_servers`. Back-compat: empty defaults.
- [ ] **T-v005-s08-02** — Three-layer discovery (`./.agent-mux/` > `~/.agent-mux/` > `<system catalog>`). New `internal/config/discovery.go`. (`StartOptions.AutoPlantBootDir=true` adoption + per-app planter deletion already landed in v005-07; no work here.)
- [ ] **T-v005-s08-03** — Skills system: `internal/skills/` package with loader + frontmatter parser + registry. Compilers for Claude (`.claude/skills/<id>.md`) and Codex (AGENTS.md inline).
- [ ] **T-v005-s08-04** — Caller-provided launch surface: service-layer accepts `agent_file`, `boot_profile`, `override`, `agent_inline`. CLI flags on `mux sessions launch`. MCP fields on `mux_session_launch`. HTTP body schema.
- [ ] **T-v005-s08-05** — `mux agents create/edit/show` CLI commands. `list` gains layer column.
- [ ] **T-v005-s08-06** — MCP per-boot-profile wiring: profile's `mcp_servers` reaches MCP proxy as `--servers <ids>` at launch.
- [ ] **T-v005-s08-07** — Catalog Mux's internal Tier 1 agents. ADR 0033 — Two-Tier Agent Config. Documentation: agent config reference, skill format spec, caller-launched-sessions guide.
- [ ] **T-v005-s08-08** — Tests, smoke (catalog-id / agent-file / inline-payload launches), `make check`, `make install`, FF-merge.

---

## Acceptance

- [ ] Schema extensions land with backward compat
- [ ] Three-layer discovery works
- [ ] Skills compile correctly for Claude + Codex
- [ ] Caller-provided launches work via CLI / MCP / HTTP / inline
- [ ] Per-launch override JSON merges correctly
- [ ] Boot profile's `mcp_servers` reaches MCP proxy
- [ ] `mux agents create/edit/show` work
- [ ] `StartOptions.AutoPlantBootDir=true` adopted; per-app planter deleted
- [ ] ADR 0033 committed
- [ ] Smoke: all three launch modes work end-to-end
- [ ] `make check` green; `make install`
- [ ] FF-merged; branch deleted; parent boot prompt updated

---

## Out of scope

- Cross-provider skill compilation for Opencode + Nanite-headless (v005-08b if needed)
- Plugin-sdk integration (post-beta)
- GUI for agent management
- ACP surface (v005-09)
- Hardening (v005-10)
- Docs push (v005-11)

---

## Notes

- Tier 1 / Tier 2 split is the architectural keystone. Don't blur it.
- Skills compilation per provider is additive — resist designing a generic IR.
- MCP per boot profile is the key unlock for Hadron-step-calls-Mux-session flows where different steps want different tool access against the same agent.
