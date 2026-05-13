# Sprint v005-03 — go-agent-sessions adoption (Phase 2)

**Epic:** post-v0.0.4 foundation work — portfolio library adoption (Phase 2: runtime layer migration)
**Status:** **SHIPPED** 2026-04-27
**Branch:** `feat/v005-03-go-agent-sessions-adoption` (FF-merged to main)
**Dependency:** `github.com/hollis-labs/go-agent-sessions v0.1.0` (shipped 2026-04-27)
**Date:** 2026-04-27

**Commits (in order):**
- Stage 2a (foundation): `2d2881c` — go-agent-sessions dep + sink adapters + claudestream factory
- Stage 2b commit 1: `e0426a3` — opencode/claudecode/stub factories alongside legacy
- Stage 2b commit 2: `091fd0b` — atomic cutover (callers switched, legacy deleted, PTY race fixed)
- Stage 2b commit 3: ADR addendums + sprint tick + boot prompt

---

## Goal

Migrate `agent-mux/internal/runtime/` and `agent-mux/internal/provider/` onto `github.com/hollis-labs/go-agent-sessions v0.1.0`. Replace mux's bespoke `Manager`, attach broker, `Runtime`/`Session` contract, and compliance harness with the library's. Keep mux's logical-agent layer (ADR 0003), HTTP API, MCP tool surface, broker/messaging, and TUI intact — those wrap the library, they don't replace it.

This is Phase 2 of the portfolio-libs adoption. Track A (go-sandbox) shipped in v005-02. Tracks B (go-runner adoption inside CLI adapters) folds into this sprint via the lib's `NewFromAdapter` substrate. Track C1 (API provider) is a separate sprint (v005-04).

**Decision lineage:** `decisions.portfolio.go_agent_sessions_composition_library`. Per the v0.1.0 wrap doc (`agent-workspaces/execution/portfolio-libs/2026-04-27/wrap.md`), three open questions are resolved here per recommendation: (1) provider-event mirroring → wrap adapter to JSON-line through Fanout, parse on consumer side; (2) state enum mapping → mux's Killed → lib's Done with Reason="killed"; (3) typed EventFanout deferred to lib v0.2.0 (tracked at Clockwork CW-20260427-0045).

---

## Tasks

- [x] **T-v005-s03-01** — Add `github.com/hollis-labs/go-agent-sessions v0.1.0` to `go.mod`. Update `go.sum`. Verify `go build ./...` succeeds (will produce many compile errors at consumer call sites — those are subsequent tasks' work, not blockers here).

- [x] **T-v005-s03-02** — Wire `*store.Store` to the library's three sink interfaces:
  - `StateSink.UpdateSessionState(id, State, pid, exit)` → existing `store.UpdateSessionState`
  - `AttachmentSink.CreateClientAttachment` / `DetachClientAttachment` → existing store methods
  - `EventSink.Emit(ctx, LifecycleEvent)` → translate to mux's `events.Publisher` envelope shape
  - State enum mapping: lib's `Done`+`Reason="killed"` → mux's `session.StateKilled` at the persistence layer; otherwise 1:1.
  Add a thin `internal/runtime/sinks.go` (or similar) that holds the three sink implementations. ~150 LOC.

- [x] **T-v005-s03-03** — Reshape per-turn adapters as `provider.CLIAdapter` impls (from `github.com/hollis-labs/go-providers/provider`). Each gets four methods: `Name()`, `BuildArgs(prompt, systemPrompt, cliSessionID)`, `ParseLine(line)`, `Detect()`.
  - `internal/provider/cli/claudestream/` → CLIAdapter shape; ParseLine delegates to `pkg/claudestream.Parse`; BuildArgs derived from current `SendInput` arg construction.
  - `internal/provider/cli/opencode/` → CLIAdapter shape; ParseLine via JSON unmarshal of `eventEnvelope`; BuildArgs from current SendInput.
  - `internal/provider/cli/goprovider/` → already wraps a `gop.CLIAdapter`; this layer collapses (the lib does the same composition now). Likely deleted entirely.
  Construct lib Runtimes via `agentsessions.NewFromAdapter(AdapterRuntimeConfig{ID, Kind:"cli", Adapter: <CLIAdapter>, Caps, ...})`.

- [x] **T-v005-s03-04** — Migrate `internal/provider/cli/claudecode/` (PTY-backed) to a direct `agentsessions.Runtime` impl. Cannot use `NewFromAdapter` (per-turn semantics) or `NewFromProvider` with go-providers' `pty_claude.go` cleanly (that's Track C2 territory). Mux owns a thin `pty_runtime.go` that satisfies `agentsessions.Runtime` directly, internally driving the PTY session through `internal/session.Handle` as today. ~200 LOC.

- [x] **T-v005-s03-05** — Provider-event mirroring (option 1 from v0.1.0 wrap). The library hands parsed `provider.StreamEvent`s to `CLIAdapter.ParseLine` and routes raw stdout bytes through `StartOptions.Fanout`. Mux needs parsed events for TUI/HTTP attach — wrap each adapter's CLIAdapter so `ParseLine` returns events AND emits a JSON-lined version through Fanout. Consumer-side parsing reuses `pkg/claudestream` + opencode JSON parser. Document the wrapping pattern in a comment header for future v0.2.0 typed-EventFanout swap.

- [x] **T-v005-s03-06** — Move compliance suite consumption from `internal/provider/compliance/` to `github.com/hollis-labs/go-agent-sessions/compliance`. Each adapter test calls `compliance.Run(t, compliance.Harness{Runtime: <constructed Runtime>})`. Delete `internal/provider/compliance/` directory.

- [x] **T-v005-s03-07** — Replace mux's `internal/runtime/Manager` with `agentsessions.Manager`. Update `internal/app/service.go` to construct the lib Manager (`agentsessions.NewManager(stateSink).WithAttachmentSink(...).WithEventSink(...)`) once at daemon start, drive sessions through it. The old `internal/runtime/manager.go`, `attach.go`, `manager_test.go`, `attach_test.go` are deleted — those concerns live in the lib now. Surfaced helpers (e.g. `RuntimeHealth`) stay in app.Service or move to a thin mux wrapper.

- [x] **T-v005-s03-08** — Update `internal/app/service.LaunchSession` to map `launch.Plan` → `agentsessions.StartRequest`:
  - `plan.Command + plan.Args + provider kind` → resolves to a constructed `agentsessions.Runtime`
  - `plan.Workdir → StartOptions.Workdir`
  - `plan.SandboxProfile → StartOptions.Profile`
  - `plan.BootPrompt → StartOptions.BootPrompt`
  - `logical_agent_id, sessionID, etc.` → `StartRequest.SessionMeta` (string map)
  - Provider session ID preset (claude --resume) → `StartOptions.SessionIDPreset` + `OnSessionID` callback
  - Fanout writer comes from mux's session log + attach broker (handled by lib internally now).

- [x] **T-v005-s03-09** — Delete `internal/provider/provider.go` (Runtime/Session/Capabilities/etc. types live in lib). Update all import sites: `provider.Runtime` → `agentsessions.Runtime`, `provider.Session` → `agentsessions.Session`, etc. Compliance check: `grep -rn "internal/provider"` after this task should only show legitimate uses (e.g. `internal/provider/cli/<adapter>/` packages).

- [x] **T-v005-s03-10** — Update ADRs:
  - **ADR 0004 (Attach broker)** — addendum noting the broker now lives in `agentsessions.Manager`. Mux still relies on the broker; the contract is unchanged. No supersede.
  - **ADR 0006 (Provider contract)** — addendum noting the contract now lives in `agentsessions.Runtime/Session`. Mux's adapters satisfy the lib's interface. No supersede.
  - **ADR 0025 (Provider compliance)** — addendum noting the harness now lives in `github.com/hollis-labs/go-agent-sessions/compliance`. Mux imports it.

- [x] **T-v005-s03-11** — `make check` green. ✓ (test-race full sweep, vet, lint 0 issues, govulncheck clean.) Live smoke awaits user verification with the installed `mux` binary against real claude / opencode CLIs.

---

## Acceptance

- [x] `internal/runtime/` directory does not exist (or is reduced to thin wiring helpers)
- [x] `internal/provider/provider.go` does not exist
- [x] `internal/provider/compliance/` does not exist
- [x] No imports of `chrispian/agent-mux/internal/runtime`, `chrispian/agent-mux/internal/provider/{provider,compliance}` remain
- [x] `go.mod` contains `github.com/hollis-labs/go-agent-sessions v0.1.0`
- [x] `make check` green; pre-existing PTY race (followups.agent_mux.compliance_resize_pty_race) likely subsumed — if not, check it's still pre-existing
- [ ] Live smoke for all 4 adapter types passes (user-verified: launch claude-stream / claude-code / opencode / api-stub against the installed binary)
- [x] ADR 0004/0006/0025 addendums committed
- [x] Conventional-commits per task; FF-merge to main; branch deleted
- [x] `make install` → `/Users/chrispian/go/bin/mux`
- [x] Boot prompt updated

---

## Design decisions

**Phase 2 minimum-viable per user direction (2026-04-27).** Track C2 (rebase mux's per-turn CLI adapters onto go-providers' built-in `subprocess_claude.go` etc.) is deferred — mux's adapters become CLIAdapter implementations internally but stay in `internal/provider/cli/`. A follow-up sprint can rebase onto go-providers' built-ins when Phase 2 is stable.

**claudecode stays as a direct Runtime impl, not via go-providers' PTYBridge.** PTYBridge (`pty_claude.go` in go-providers) wraps claude as a `provider.Provider` for full chat-style streaming. Mux uses claudecode for long-lived PTY sessions where we own the PTY directly via `internal/session.Handle`. Migrating to PTYBridge means giving up that direct PTY ownership — significant rework not justified by this sprint's scope.

**Provider-event mirroring → option (1).** Wrap the CLIAdapter so `ParseLine` returns events AND emits JSON-lined bytes through Fanout. Consumer (mux TUI, HTTP attach) parses bytes back to events using existing `pkg/claudestream` parser. Clockwork CW-20260427-0045 tracks the v0.2.0 improvement (typed `EventFanout chan provider.StreamEvent`) — pick it up after we feel the JSON-line-then-reparse pattern is doing real work.

**State enum mapping.** Lib has 4 values; mux's `session.State` has more (Killed, Faulted, etc.). The translation lives in `StateSink.UpdateSessionState`: lib emits Done with Reason="killed" → mux persists as Killed. No information loss.

**Sinks injected at daemon startup, not per-session.** Single `agentsessions.Manager` instance owned by `app.Service`; sinks bound to mux's `*store.Store`.

**ADR addendums, not supersedes.** ADR 0004/0006/0025 contracts move to the library verbatim. Mux still consumes them — the contract is preserved, the implementation address changed.

---

## API delta consumers absorb

| mux today | After Phase 2 |
|---|---|
| `internal/runtime.Manager` | `agentsessions.Manager` (composed in app.Service) |
| `internal/provider.Runtime` | `agentsessions.Runtime` (lib type) |
| `internal/provider.Session` | `agentsessions.Session` (lib type) |
| `internal/provider.StartOptions` | `agentsessions.StartOptions` |
| `internal/provider.Capabilities` | `agentsessions.Capabilities` |
| `internal/provider/compliance` | `github.com/hollis-labs/go-agent-sessions/compliance` |
| Custom subprocess plumbing in claudestream/opencode/goprovider | `agentsessions.NewFromAdapter` + `runner.Run` per turn |
| events.Publisher embedded inline in Manager | `EventSink.Emit` interface, sink-side translation to events.Publisher |

---

## Sequencing & risk

**Risk: large rip-and-replace PR.** ~30 files touched. Mitigation: tasks are roughly independently committable (dep add → sinks → CLIAdapter reshapes → claudecode direct impl → compliance move → Manager swap → deletes → ADRs). Each task can land on the branch as a separate commit, FF-merge at end.

**Risk: pre-existing PTY race.** Sprint v005-02 surfaced `TestCompliance/Baseline/ResizeNoOpOrNoError` as a pre-existing race in `internal/session/runtime.go` PTY teardown. The Phase 2 migration moves Manager + most callers, so the race conditions may auto-resolve OR they may follow `internal/session.Handle` into mux's direct claudecode Runtime impl. Re-test after T-v005-s03-04 lands.

**Risk: lib v0.1.0 may have rough edges.** First real consumer; expect to file issues against go-agent-sessions. Worst case: pin to a local replace directive temporarily, file the issue, push fix, unpin.

---

## Out of scope (deferred)

- **Track C2:** rebase mux's per-turn adapters onto go-providers' built-in CLIAdapters (e.g. `subprocess_claude.go`). Future sprint.
- **claudecode → PTYBridge.** Future sprint with Track C2.
- **C1 API provider.** Sprint v005-04 (depends on Phase 2 stable).
- **Typed `EventFanout`.** v0.2.0 of go-agent-sessions. Tracked at Clockwork CW-20260427-0045.
- **clockwork / nanite migration.** Per-app sprints, owned by their teams. Tracked at `followups.portfolio.go_agent_sessions_consumer_migrations`.
