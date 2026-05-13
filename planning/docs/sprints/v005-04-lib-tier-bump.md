# Sprint v005-04 — Lib Tier Bump + Provider Migration

**Epic:** post-v0.0.4 foundation work — portfolio library adoption catch-up after the 2026-05-08/09 reshape
**Status:** active, but narrowed to substrate catch-up
**Branch:** `feat/v005-04-lib-tier-bump`
**Dependency:** v005-03 (Phase 2 mux-on-go-agent-sessions migration) shipped; `main` tip `f80fe98`
**Date:** 2026-05-09

---

## Goal

Bring `agent-mux` up to the current portfolio lib tier and remove the app-local substrate that those libs now own. This sprint bumps to `go-providers v0.13.0`, `go-agent-sessions v0.7.1`, `go-sandbox v0.2.0`, `go-runner v0.4.0`; deletes `internal/provider/cli/claudecode/`; adopts `BootDirSpec()` for boot-dir planting; moves Claude long-lived sessions onto `provider.NewClaudeAdapterPTY()` plus `agentsessions.Manager.Start`; and records the parked C1 API-provider reframe in ADR 0029.

**2026-05-11 reframe:** Mux is now explicitly a **long-lived CLI session control plane**. Codex/Opencode one-shot semantics are no longer the target end state for Mux. This sprint therefore closes as a substrate/lib-tier catch-up even if transitional turn-based provider paths remain for a short time; the long-lived provider unification work moves into v005-05.

**Why now:** the portfolio reshaped across 2026-05-08/09. `go-providers` no longer ships HTTP API providers, but the CLI/PTY surface mux actually uses remains and is richer. Mux has no HTTP LLM consumers to migrate, so the correct v005-04 work is the lib-tier catch-up plus deletion of now-redundant app code.

**Long-term reinforcement:** the original C1 plan is no longer structurally valid. Future API-provider work in mux will use per-app SDK wrappers in `internal/llm/<vendor>/` implementing `go-llm-contracts`, as captured in ADR 0029 and the portfolio migration guide.

---

## Tasks

- [ ] **T-v005-s04-01** — Bump `go.mod` and `go.sum` to `go-providers v0.13.0`, `go-agent-sessions v0.7.1`, `go-sandbox v0.2.0`, `go-runner v0.4.0`; run `go mod tidy`; capture first-pass `go build ./...` fallout.

- [ ] **T-v005-s04-02** — Delete `internal/provider/cli/claudecode/` entirely and cut mux over to the lib PTY substrate for Claude long-lived sessions (`provider.NewClaudeAdapterPTY()`, `agentsessions.Manager.Start`, `AutoFireFirstTurn`, `WorkspaceDir`, typed events, `PIDReporter`).

- [ ] **T-v005-s04-03** — Adopt `BootDirSpec()` boot-dir planting for Claude, Codex, Opencode, and Claudestream where covered. Preserve stub-spec exclusion. Treat `bootstrap.mode: agents_md` as a tolerated no-op rather than a schema break.

- [ ] **T-v005-s04-04** — Update sandbox usage for `Profile.AllowLoopback=true` in the workspace-plus-net profile and ensure `*ExitError.Cause` propagates through mux session state and health reporting.

- [ ] **T-v005-s04-05** — Triage any TUI compile breakage caused by deleting claudecode by removing the broken TUI surface instead of patching it. `mux tui` is allowed to stay broken or disappear; daemon and MCP paths must build cleanly.

- [ ] **T-v005-s04-06** — Write ADR 0027 (lib tier adoption), ADR 0028 (`BootDirSpec` adoption), ADR 0029 (API-provider reframe), and add the ADR 0013 loopback addendum. Update the parent boot prompt working state after the repo lands.

- [ ] **T-v005-s04-07** — Run `make check`, execute smoke for Claude PTY long-lived and Claude bare-mode via `apiKeyHelper`, record any Codex/Opencode transitional results as evidence only, FF-merge, and `make install`.

---

## Acceptance

- [ ] `go.mod` pins `go-providers v0.13.0`, `go-agent-sessions v0.7.1`, `go-sandbox v0.2.0`, `go-runner v0.4.0`
- [ ] `internal/provider/cli/claudecode/` no longer exists
- [ ] Claude long-lived sessions run through lib PTY support with workspace-dir logging and first-turn boot delivery
- [ ] Codex and Opencode boot planting comes from `BootDirSpec()`; Claudestream does too if the lib covers it, otherwise the gap is documented
- [ ] Workspace-plus-net sandbox enables loopback
- [ ] `make check` is green
- [ ] Smoke passes for Claude PTY and Claude bare-mode; any Codex/Opencode one-shot findings are captured as transitional evidence and explicitly deferred to the long-lived-only follow-up sprint
- [ ] ADR 0027, 0028, 0029 written; ADR 0013 addended
- [ ] Conventional commits per task; FF-merge; branch deleted; boot prompt updated

---

## Design decisions

**Delete app-local PTY substrate, do not wrap it.** `internal/provider/cli/claudecode/` seeded the library and is now redundant. Full delete is cleaner than any compatibility shim.

**TUI breakage is not a rescue target.** This sprint preserves the daemon, MCP, and provider paths. TUI code that only exists to keep the abandoned surface compiling can be removed. The replacement UI is the GUI, not more TUI work.

**Skip the HTTP migration arc entirely.** Mux has no current HTTP LLM consumers. The post-reshape API-provider direction is recorded in ADR 0029, not implemented here.

**`apiKeyHelper` is part of the bump, not an app feature.** Claude bare-mode support comes from the upgraded adapter surface in `go-providers v0.13.0`.

---

## Out of scope (future sprints)

- **Any HTTP/API provider implementation.** Future C1 work will be a separate sprint built on `internal/llm/<vendor>/` wrappers over vendor SDKs.
- **Long-lived provider unification.** Sequenced as v005-05 after this substrate migration lands.
- **`go-llm-contracts`, `go-llm-types`, `go-embed-contracts` adoption.** Reserved for the future API-provider sprint.
- **TUI modernization.** The replacement is a future HTML/JS GUI, not more Bubble Tea patching.
