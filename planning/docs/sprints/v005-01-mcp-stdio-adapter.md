# Sprint v005-01 — MCP stdio Adapter

**Epic:** post-v0.0.4 foundation work  
**Status:** SHIPPED  
**Landed:** `1fc8397` (feat/mcp-stdio-adapter → to be FF-merged to main)  
**Date:** 2026-04-21  

---

## Goal

Expose the agent-mux runtime as MCP tools over stdio so LLM-based tools
(Claude Code, Codex, Kiro, Hadron blueprints, custom agents) can call
session lifecycle, catalog reads, messaging, and boot prompt generation
directly from tool calls.

---

## Tasks

- [x] **T-v005-s01-01** — Add `mark3labs/mcp-go v0.47.0` dependency; skeleton `internal/mcpadapter/adapter.go` (Adapter, New, Run, scope check, helpers)
- [x] **T-v005-s01-02** — Health + catalog tools (`mux_health`, `mux_catalog_list_projects/agents/providers/launches/boot_profiles`)
- [x] **T-v005-s01-03** — Session tools (`mux_session_list/get/create/launch/stop/wait/send_input/resize`)
- [x] **T-v005-s01-04** — Logical agent + boot tools (`mux_logical_agent_list/resume`, `mux_boot_generate`)
- [x] **T-v005-s01-05** — Messaging tools (`mux_message_send/get/inbox/thread/consume/cancel`)
- [x] **T-v005-s01-06** — `cmd/mux/mcp.go` cobra subcommand + `make check` green
- [x] **T-v005-s01-07** — ADR 0019 + sprint file + boot prompt update

---

## Acceptance

- [x] `mux mcp` starts and responds to `tools/list` over stdio
- [x] All 23 tools registered with correct descriptions and parameter schemas
- [x] Read-only tools (health, catalog, session reads, message reads) require no auth
- [x] Mutating tools gate on `session.write` / `message.write` scope
- [x] Token + scopes configurable via flags or env vars
- [x] `make check` green (448 tests, 1 skipped, pre-existing tui failure unrelated)
- [x] ADR 0019 written
- [x] No regressions on existing test suite

---

## Design decisions

See `docs/adr/0019-mcp-stdio-adapter.md` for the full decision record.

Key: in-process `app.Service` wrapping (not proxy over UDS), mark3labs/mcp-go
v0.47.0, token+scope gating, tool naming convention `mux_<group>_<verb>`.

---

## Backlog / follow-ups

- **[MCP-STREAM]** Streaming PTY attach output via MCP — deferred; requires a
  different transport model. Investigate MCP streaming or a polling pattern.
- **[MCP-PROXY]** If SQLite WAL contention from concurrent adapter+daemon writes
  becomes an issue, add a proxy-over-UDS mode routing through the daemon's HTTP
  API. Interface unchanged; transport swapped.
- **[MCP-BROKER]** Expose legacy `/broker/*` routes as tools if consumers need
  them. Currently only `/messages/*` (go-messaging native) is wired.
- **[MCP-CATALOG-WRITE]** Tools to register/update catalog entries at runtime
  (launch profiles, agents, providers). Useful for programmatic catalog management.
