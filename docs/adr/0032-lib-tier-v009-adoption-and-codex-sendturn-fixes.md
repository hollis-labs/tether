# ADR 0032 — Lib-tier v0.9.x adoption + Codex SendTurn protocol fixes

**Status:** Accepted
**Date:** 2026-05-12
**Supersedes:** —
**Superseded by:** —

## Context

`go-agent-sessions v0.9.0` absorbed the per-app BootDirSpec planter into the lib's runtime — apps opt in via `StartOptions.AutoPlantBootDir=true` and the lib materializes the adapter's planted files, applies bare-mode injection (for `*ClaudeAdapter` with `Bare: true`), threads `EnvAmendments` / `ExtraArgs` / spawn `cwd`, fires an `OnBootDirPlanted` callback, and cleans up on every terminal-state exit path. The same release added `Manager.JsonRpcCall` so Manager-mediated consumers can reach the per-session `JsonRpcCaller` interface without breaking the "no raw `Session` exposure" invariant (`go_agent_sessions_manager_capability_dispatch_pattern`). `v0.9.1` fixed `ProviderSessionID()` to return `SessionIDPreset` before the first agent-observed event; `v0.9.2` added an abnormal-wait stderr diagnostic for long-lived runtimes that exit suspiciously fast.

Mux's `v005-05` (long-lived integration) shipped on `v0.8.0` with three known follow-ups:

1. Mux carried its own per-app planter in `internal/provider/cli/claudestream/cliadapter.go` (`plantBootDir` + helpers — `bootDirLayout`, `bootDirRoot`, `substituteTemplates`, `substituteArgTokens`, `sanitizeID`, `bootDirSession` wrapper, plus bare-mode injection).
2. Codex `SendTurn` was implemented via `Manager.JsonRpcCall` (lazy `initialize` → `thread/start` → `turn/start`) but never live-smoked.
3. `claude-stream` provider's `Caps.ProviderSessionID = true` exposed a latent v0.8.0 bug (`lastSessionID` not pre-seeded from `SessionIDPreset`) that v0.9.1 then fixed upstream.

This sprint (`v005-07`) closes the lib-adoption substrate gap end-to-end.

## Decision

### 1. Adopt `go-agent-sessions v0.9.2`

Pin already at `v0.9.2` from a prior cycle. Set `StartOptions.AutoPlantBootDir = true` at the single `LaunchSession` Options site in `internal/app/service.go`. All five runtime factories (`claude-stream`, `claude-code`, `codex-app-server`, `opencode`, catalog-driven `cli-goprovider`) funnel through a single `agentsessions.StartRequest` block, so flipping the flag once covers every launch path — no per-factory threading needed.

### 2. Wire `OnBootDirPlanted` to the mux event bus

New event kind `events.KindSessionBootDirPlanted = "session.boot_dir_planted"` (additive — `internal/events/kinds.go`). Payload schema `{"path":"<absolute>"}`. The callback is built by `makeBootDirPlantedCallback(bus, sessionID, logicalAgentID)` (`internal/app/sinks.go`) and passed via `StartOptions.OnBootDirPlanted`. Lib invokes it once per session start after the planted bootdir is materialized; the callback publishes the event to the bus, which persists it to the `events` table and fans it to attach observers.

This gives operators and downstream tooling a stream-able record of "where did this session's planted boot config live" without scraping filesystem state.

### 3. Delete mux's per-app planter

Strip `plantBootDir`, `bootDirLayout`, `bootDirRoot`, `substituteTemplates`, `substituteArgTokens`, `sanitizeID`, `bootDirSession`, the custom `runtime.Start` override, the bare-mode injection block, and the unconditional `PlanScopedAdapter.Clone()` in `internal/provider/cli/claudestream/cliadapter.go`. Lib v0.9.x is functionally equivalent for our use cases (mux never used bare-mode Claude adapters — `NewClaudeAdapterStreamingStdio()` and `NewClaudeAdapter()` both have `Bare: false` — so the bare-mode branch was effectively dead).

`claudestream` package itself is retained because:

- `PlanScopedAdapter` (catalog binary + base-args wrap) remains load-bearing for five different runtime factories.
- The wrapper's `BootDirSpec()` forwarder is required for the lib's `BootDirProvider` discovery to see through to inner adapters.
- The package name is anachronistic (no longer a streaming wrapper) but renaming touches every consumer; deferred as `followup_claudestream_pkg_rename`.

`NewWithAdapter` collapses to a direct `agentsessions.NewFromAdapter` call.

### 4. Codex `SendTurn` protocol fixes

The v005-05 `sendTurnJSONRPC` implementation had three protocol-shape errors against codex 0.130.0's actual schema (surfaced for the first time by this sprint's live smoke):

| Surface | v005-05 shipped shape | Correct shape (per `codex app-server generate-json-schema`) |
|---|---|---|
| `initialize` params | `{}` | `{clientInfo: {name, version}}` (required) |
| `thread/start` response decode | `{threadId: string}` | `{thread: {id: string, ...}}` |
| `turn/start` `input` | `<plain-string>` | `[{type: "text", text: <string>}]` |

The first error returned `Invalid request: missing field 'clientInfo'`; the second returned an empty thread id; the third would have rejected the turn payload. Fixed inline in `internal/app/service.go`'s `sendTurnJSONRPC`; new build-time constant `muxClientVersion = "v005-07"` (the value is reported to codex for diagnostic identification only).

### 5. Catalog fix: codex provider double-stack

`~/.agent-mux/catalog/providers/codex-app-server.yaml` had `args: [app-server]` while `provider.NewCodexAdapterAppServer().BuildArgs(...)` already emits `["app-server"]`; resulting argv was `codex app-server app-server`, which codex rejected. Fixed to `args: []` (mirroring the `claude-code` / `claude-stream` shapes). Catalog change only; no code impact.

### 6. Upstream fix consumed: `go-providers v0.17.1`

`CodexAdapter.BootDirSpec()` emitted `ProjectDirArg: "--cd {{.ProjectDir}}"` unconditionally; `codex app-server` rejects `--cd` (it's an `exec`-mode flag). Lib v0.9.x's AutoPlantBootDir faithfully appended it via `ExtraArgs`, so every codex launch via Mux exited code 2 ~10ms after spawn. A parallel session shipped `go-providers v0.17.1` with a mode-aware switch (`ProjectDirArg: ""` for `app-server`); mux consumes the bump (`go get -u github.com/hollis-labs/go-providers@v0.17.1 && go mod tidy`).

### 7. Diagnostic value of v0.9.2 abnormal-wait logging

The v0.9.2 stderr diagnostic surfaced exactly the right signal during the codex debug pass:

```
agentsessions: jsonrpc-stdio waiter abnormal: session=codex-app-server pid=87976 elapsed=12.122ms err_type=*exec.ExitError err="exit status 2"
```

This is the design intent — high-signal stderr lines for the abnormal-exit bug class without noise during normal operation. The diagnostic was load-bearing for this sprint and is worth preserving in future hardening passes.

## Consequences

- Mux no longer carries per-app planting logic. `grep -rln "plantBootDir|MkdirTemp.*boot" internal/ cmd/` returns zero — the grep guard pins this.
- Codex app-server is functionally complete via Mux: live 2-turn smoke verified the same `threadId` across CLI + HTTP turns (the MCP path routes through the same `Service.SendTurn` confirmed by code review).
- Claude streaming-stdio remains KV-cache-hit on turn 2 (`cache_read_input_tokens=31323` vs `cache_creation=31323` on turn 1) — no regression from v005-05.
- The `claudestream` package's footprint shrinks from 352 lines to ~85 lines while still owning the catalog-binary + plan-args wrapping for all five factories.
- Bare-mode Claude adapters, if/when mux uses one, will get the lib's automatic per-session clone + `BareInjectionPaths` application — strictly an improvement over the old in-package logic.

## Out of scope

- `mux mcp` daemon proxy fix (`followup_mux_mcp_proxy_session_mutating_tools_to_daemon` — pre-existing architectural gap).
- Session-id store namespacing fix (`followup_mux_session_id_store_namespacing` — Codex's `threadId` and Claude's `session_id` both flow through the store keyed by `logical_agent_id`; cross-provider collision risk noted but not blocking).
- `claudestream` package rename (the package is no longer a streaming wrapper; deferred).
- Agent Ops two-tier config (`v005-08`).
- ACP surface (`v005-09`).
- Hardening audit (`v005-10`).

## References

- `agent-workspaces/execution/agent-mux/v005-07-lib-tier-adoption/2026-05-12/smoke-results.md` — full live-smoke evidence.
- `agent-workspaces/knowledge/portfolio/cli-agent-long-lived-modes.md` — 4-mode lifecycle axis the v0.9.x adoption realizes end-to-end.
- `agent-workspaces/boot/agent-mux/boot-prompt-v005-07-lib-tier-adoption.md` — sprint scope (note: phases 1 and 4 were absorbed by v005-05 before this sprint started; this sprint executed phases 2/3/5/6 + the catalog fix + protocol-shape fixes).
- Vanta `followup_go_agent_sessions_v009_absorb_bootdirspec_planting` — upstream ship record.
- Vanta `go_agent_sessions_manager_capability_dispatch_pattern` — pattern this sprint exercises end-to-end for the first time.
- Vanta `followup_go_providers_codex_app_server_projectdirarg_fix_shipped` — go-providers v0.17.1 ship record.
- Vanta `mux_beta_push_roadmap_may_2026` — sprint sequence positioning v005-07 between TUI removal (v005-06) and Agent Ops (v005-08).
