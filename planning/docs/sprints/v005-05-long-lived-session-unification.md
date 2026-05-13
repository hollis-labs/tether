# Sprint v005-05 — Long-Lived Headless Integration

**Epic:** post-v0.0.4 foundation work — narrow Mux to its long-lived CLI session control-plane role
**Status:** staged (libs v0.17.0 / v0.8.0 shipped; integration ready to start)
**Branch:** `feat/v005-05-long-lived-integration`
**Dependency:** go-providers v0.17.0 + go-agent-sessions v0.8.0 — **shipped** 2026-05-11
**Date:** 2026-05-11 (rescoped after the long-lived investigation)

---

## Goal

Adopt the v0.17.0 / v0.8.0 lib tier and switch Mux's Claude launch from PTY/TUI to **streaming-stdio** (`Claude --input-format stream-json` mode, one long-lived process, NDJSON over stdio, KV-cache reused across turns). Add Codex via **JSON-RPC stdio** (`codex app-server`). Delete `internal/provider/cli/claudestream/` (the custom runtime wrapper that currently fails with `exit -1`). Drop PTY-specific sandbox augmentations. Live smoke proves a long-lived Claude session over Mux CLI/MCP/HTTP with KV-cache hits across turns.

TUI removal, Agent Ops, ACP, hardening, docs are sequenced as v005-06 → v005-10 respectively.

---

## Sub-boot-prompt

`~/Projects-apps/agent-workspaces/boot/agent-mux/boot-prompt-v005-05-long-lived-integration.md` — read end-to-end. Carries every concrete API name, decision, and acceptance criterion.

---

## Tasks

- [ ] **T-v005-s05-01** — Bump `go.mod` to `go-providers v0.17.0` and `go-agent-sessions v0.8.0`; `go mod tidy`; capture compile fallout.
- [ ] **T-v005-s05-02** — Delete `internal/provider/cli/claudestream/`. Re-home `PlanScopedAdapter` if it remains useful.
- [ ] **T-v005-s05-03** — Rewrite `newClaudeCodeRuntime` to use `gop.NewClaudeAdapterStreamingStdio()` + `Caps.StreamingStdio=true` + `agentsessions.NewFromAdapter` directly (no wrapper).
- [ ] **T-v005-s05-04** — Add `newCodexAppServerRuntime` using `gop.NewCodexAdapterAppServer()` + `Caps.JsonRpcStdio=true`. Map Codex JSON-RPC notifications (`turn/started`, `item/*`, `turn/completed`) to Mux's event bus.
- [ ] **T-v005-s05-05** — Service-layer `SendTurn(ctx, sessionID, text)` method that dispatches on session caps: streamingStdio → NDJSON user-message envelope → Manager.SendInput; jsonRpcStdio → `JsonRpcCaller.Call("turn/start", ...)` after a lazy `initialize` + `thread/start`. Wire CLI / MCP / HTTP to use it.
- [ ] **T-v005-s05-06** — Delete PTY-specific blocks in `service.go`: `AutoFireFirstTurn`/`FirstTurnPayload` conditional, `augmentClaudeCodePTYSandbox`, `augmentPTYEnv`.
- [ ] **T-v005-s05-07** — Update catalog YAML: `claude-code.yaml` bootstrap semantics; add `codex-app-server.yaml`. ADR 0030 — long-lived lifecycle integration.
- [ ] **T-v005-s05-08** — Tests for runtime construction + SendTurn framing + JSON-RPC routing; race-clean. End-to-end live smoke proving KV-cache hits across turns (Claude) and same thread ID across turns (Codex). All three surfaces (CLI, MCP, HTTP). `make check` green. `make install`. FF-merge to main; delete branch.

---

## Acceptance

- [ ] `go.mod` at v0.17.0 / v0.8.0
- [ ] `internal/provider/cli/claudestream/` deleted
- [ ] Claude launch uses streaming-stdio runtime kind via direct `agentsessions.NewFromAdapter`
- [ ] Codex launch uses jsonRpcStdio runtime kind
- [ ] Service-layer `SendTurn` exists and routes per session caps
- [ ] PTY-specific augmentations + AutoFireFirstTurn block deleted from service.go
- [ ] ADR 0030 committed
- [ ] **Smoke: KV-cache hit on turn 2** of Claude streaming session (`cache_read_input_tokens > 0`)
- [ ] **Smoke: same Codex thread ID across 2 turns**
- [ ] Smoke verified once each via CLI, MCP, HTTP
- [ ] `make check` green; `make install` to `/Users/chrispian/go/bin/mux`
- [ ] FF-merged to main; branch deleted; parent boot prompt updated

---

## Out of scope

- TUI removal (v005-06)
- Agent Ops config-layer unification (v005-07)
- ACP surface (v005-08)
- Hardening / modernization (v005-09)
- Docs push (v005-10)
- HTTP/API LLM providers (ADR 0029 stands)
- Opencode + other providers
- Resume / checkpoint behavior changes (existing path stays)

---

## Notes

- The TUI is allowed to break during this sprint if it depends on claudestream or PTY-only types. **Delete the broken TUI file**, do not patch. `mux tui` subcommand is going away in v005-06 anyway.
- The KV-cache hit on turn 2 is the load-bearing smoke. If it's 0, the session isn't actually long-lived — investigate before claiming green.
