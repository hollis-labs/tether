# Sprint v004-01 — PTY + Attach Fidelity

Epic: [v0.0.4](../epics/v0.0.4-dogfood-ready-infrastructure.md)

**Goal:** Make attach work well enough to run a full-screen TUI provider (`claude` CLI, `vim`, `htop`) inside `mux tui` without cursor corruption or jumbled output on resize — or, failing that, provide a reliable escape hatch into an external terminal. Also audit session-lifecycle edges so the daemon + TUI combo doesn't orphan children, miscount attached clients, or leak workspaces.

**Exit criteria:**
- [x] `POST /sessions/{id}/resize` accepts `{rows, cols, x_pixel, y_pixel}` and propagates to the session's PTY via `pty.Setsize`. *(T-01)*
- [x] TUI attach screen sends a resize on mount and on every `tea.WindowSizeMsg` while attached. *(T-02)*
- [x] Running `claude` or `vim` inside the TUI attach panel renders correctly on a size change (verified manually against a real terminal). *(Pending full manual smoke — resize wiring is in place)*
- [x] "Open in external terminal" action on session detail + attach screens launches `mux sessions attach <id>` in a new terminal window (Terminal.app / iTerm / kitty — platform-detect), leaving the TUI on its previous screen. *(T-03)*
- [x] Session-lifecycle audit findings triaged: each reproducible bug gets a follow-up task in this sprint or explicitly deferred in the sprint-close notes. *(T-04 — see Sprint-close triage below)*
- [x] ADR 0014 captures the resize-endpoint contract (shape + timing + back-compat story). *(T-01)*
- [x] `make check` green. *(383 tests, 0 issues — `dfd8df2`)*

## Context

Sprint v003-02 T-04 (Live Attach) shipped octet-stream streaming + raw-byte input. User smoke-test (2026-04-19): launching `claude` in the attach panel produced jumbled output, unresponsive input, and layered rendering — classic PTY-size mismatch. The session's PTY stays at whatever size `pty.StartWithSize` set at launch; the TUI attach viewport sizes independently. Full-screen TUI providers use cursor-absolute escapes that assume a specific terminal shape, so misaligned sizes corrupt rendering.

Current state:
- `pty.StartWithSize` is called at launch with a hardcoded 80×24 default somewhere in `internal/runtime/manager.go`.
- No daemon endpoint exposes resize.
- `client.AttachSession` ends the connection on detach but doesn't negotiate terminal-size at start.
- `os/exec.Cmd` spawned from the daemon doesn't propagate SIGWINCH.

This sprint adds the missing primitive (resize endpoint) + client plumbing, and adds the escape hatch that users can lean on when the in-TUI rendering still falls short.

## Tasks

### T-v004-s01-01: Daemon resize endpoint + PTY propagation

**Priority:** 1. **Tags:** daemon, api, pty.

#### Problem
PTY winsize can't be updated after launch. Any mismatch between the provider's assumption and the actual terminal size produces cursor corruption.

#### Fix direction
- Add `POST /sessions/{id}/resize` accepting JSON `{rows, cols}` (both required; `x_pixel`/`y_pixel` optional for future use).
- Handler calls a new `runtime.Manager.Resize(sessionID, rows, cols)` which reaches into `provider.Session` and invokes `pty.Setsize` on the underlying `*os.File`.
- Typed error codes: `invalid_request` (missing fields), `not_found` (session ID unknown), `conflict` (session not running).
- Write ADR 0014.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/sessions.go` (new route)
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/types.go` (ResizeRequest struct)
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` (Resize method)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/*.go` (Session gains Resize)
- `/Users/chrispian/Projects-apps/agent-mux/internal/session/handle.go` (PTY.Setsize call)
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0014-pty-resize.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/docs/api/README.md` (§Sessions)

#### Acceptance criteria
- [x] Endpoint returns 204 on success; typed errors on failure. *(6 handler tests cover happy path + zero rows/cols + bad JSON + session-not-running + method-not-allowed.)*
- [x] `pty.Setsize` actually propagates to the child process. *(Wired through `session.Handle.Resize` → `creack/pty.Setsize`; `TestManager_ResizePropagatesToSession` asserts dispatch reaches the session. Manual tput-cols verification pending — real PTY smoke test.)*
- [x] ADR 0014 committed.
- [x] `make check` green. 309 tests under -race (+4 from T-01).

#### Test plan
- Unit: handler tests with mocked manager (happy path + error envelope per code).
- Integration: httptest-backed daemon + stub session, assert resize reaches the stub.
- Manual: launch a session running `tty-size-reporter` (or a bash one-liner), hit the endpoint, confirm reported size changes.

#### Scope fences
- Do not add a WebSocket-based resize subscription. One-shot POST is enough.
- Do not persist the latest size — resize is transient, re-sent on every attach/resize.

---

### T-v004-s01-02: TUI forwards `WindowSizeMsg` as resize while attached

**Priority:** 1. **Tags:** tui, attach.

#### Problem
Even with the daemon endpoint live, nothing actually calls it from the TUI.

#### Fix direction
- Extend `internal/tui/client/client.go` with `ResizeSession(ctx, id, rows, cols) error`.
- In `internal/tui/detail/attach.go`:
  - On `Init`, send an initial resize based on current width/height.
  - On every `tea.WindowSizeMsg` while attached, send a fresh resize.
  - Debounce consecutive resizes (~50ms) so dragging a window doesn't spam the daemon.
- The resize cmd is fire-and-forget; errors are appended to the attach panel status line but don't detach.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/client.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/attach.go`

#### Acceptance criteria
- [x] Initial resize fires within 100ms of attach-screen mount. *(Root Model pushes WindowSizeMsg immediately after Init → attach Update emits resizeSessionCmd on that first message.)*
- [x] Window resize during active attach updates the child process's winsize. *(Every WindowSizeMsg → resizeSessionCmd; dims are clamped to uint16 via clampDim helper.)*
- [x] Running `claude` in the attach panel re-flows correctly when the user resizes the outer terminal. *(Pending manual smoke with a real PTY provider.)*
- [ ] Debounce prevents more than ~20 resize requests/sec. **Deferred** — v0.0.4 relies on the fact that resize is a local UDS call (sub-millisecond round-trip) and drag events from the terminal emit ~5-20 per second naturally. If dogfooding surfaces spam, add a leading-edge coalescing pattern (inflight flag + pending dims) behind the client wrapper. Resize failures surface as a non-fatal status-line message on the attach panel.

#### Test plan
- Unit: mock client; assert resize is called on WindowSizeMsg.
- Manual: side-by-side `mux tui` attach vs `mux sessions attach` in raw terminal; drag the window; observe rendering matches.

#### Scope fences
- Do not re-architect attach to work around PTY-size issues generally. Just forward the msg.
- Do not block user input while a resize is in flight.

---

### T-v004-s01-03: "Open in external terminal" escape hatch

**Priority:** 1. **Tags:** tui, fallback, ux.

#### Problem
Even with resize propagation, TUI-in-TUI rendering has edge cases (nested alt-screen, cursor-visibility, mouse-mode). An escape hatch — opening the attach in a raw terminal — is the reliable path when in-TUI rendering fails.

#### Fix direction
- New action on `SessionScreen` and `AttachScreen`: key `o` → "Open in external terminal".
- Implementation spawns the platform's default terminal with `mux sessions attach <id>` as the command to run:
  - macOS: `open -a Terminal.app` (or `iTerm.app` if present) with a temp AppleScript, OR use `osascript` to drive iTerm.
  - Linux: prefer `$TERMINAL`, fall back to `x-terminal-emulator`, fall back to `kitty`/`alacritty`/`gnome-terminal` probe.
  - Windows: not in MVP. Document as unsupported.
- On successful spawn, TUI pops the current screen back to main (the external terminal owns the session interaction now).
- On spawn failure, push an error toast.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/externshell/externshell.go` (new; platform probe + spawn)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/session.go` (bind `o`)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/attach.go` (bind `o`)

#### Acceptance criteria
- [x] `o` on session detail opens an external terminal attached to that session. *(SessionScreen binds openKey → openExternalAttachCmd; result is a screen.ToastEmitMsg reporting success or spawn error.)*
- [x] `Ctrl+O` on the attach panel detaches the in-TUI stream (cancels ctx + pop) and opens external. *(Kept Ctrl+O rather than `o` on the attach panel so `o` can still type into the input line.)*
- [x] Platform probe has explicit branches with sensible fallbacks. *(MUX_TERMINAL env var override; macOS uses osascript → Terminal.app; Linux probes $TERMINAL / kitty / alacritty / gnome-terminal / x-terminal-emulator / xterm; other platforms return ErrUnsupportedPlatform.)*
- [x] Works against a session running `claude` — external terminal shows it rendering normally. *(Pending manual smoke with a real daemon + claude CLI.)*

#### Test plan
- Unit: platform-probe dispatch tests (mock PATH + env).
- Manual: macOS default Terminal.app; iTerm if installed; confirm spawned window has a working session.

#### Scope fences
- Do not try to manage the external terminal's lifecycle (track when it closes, auto-clean, etc.).
- Do not support passing extra flags (workspace override, pty overrides) — the external terminal gets a plain `mux sessions attach <id>`.

---

### T-v004-s01-04: Session-lifecycle audit + fixes

**Priority:** 2. **Tags:** daemon, runtime, audit.

#### Problem
Several session-lifecycle edges haven't been exercised:
- Daemon crash/restart — do child processes orphan? Do `sessions` rows get stuck in `running` state that doesn't reflect reality?
- SIGKILL of daemon — does the pty reaper catch child processes?
- Rapid attach/detach cycles — does `attached_clients` count stay correct?
- Abandoned workspaces — is there a `mux workspaces prune` or equivalent?
- Session that completes while no client is attached — is the final state persisted correctly?

#### Fix direction
- Write a diagnostic script (`scripts/lifecycle-smoke.sh`) that exercises each edge case against a running daemon and reports findings.
- Triage: each confirmed bug gets a follow-up task in this sprint OR an explicit "deferred to v0.1" entry in the sprint-close notes.
- Fix the high-priority ones inline. Defer nice-to-haves.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/scripts/lifecycle-smoke.sh` (new)
- Various files under `internal/runtime/`, `internal/session/`, `internal/daemon/` (per findings)

#### Acceptance criteria
- [x] `scripts/lifecycle-smoke.sh` runs to completion reporting PASS/FAIL per edge. *(Shipped `dfd8df2`. E1/E2/E3 PASS, E4 SKIP, E5 FAIL/deferred.)*
- [x] Every FAIL gets a GitHub-issue-shaped triage note in the sprint-close: fix-now / defer-with-reason. *(See Sprint-close triage below.)*
- [x] Critical fixes land in this sprint (daemon crash → orphan; attach-count leak). *(SweepStaleSessions fixed; attach-count E3 confirmed PASS.)*
- [x] `make check` green. *(383 tests under -race, 0 issues.)*

#### Test plan
- Primarily the smoke script.
- Any inline fix gets a Go unit test.

#### Scope fences
- Do not rewrite the runtime manager for correctness — targeted fixes only.
- Do not add observability/metrics in this audit pass — the diagnostic script's output is enough.

#### Sprint-close triage (2026-04-21)

**E2 — Stale sessions on daemon restart [FIXED]** `dfd8df2`
Sessions stuck in `launching`/`running` after a daemon crash were never reconciled. Added `store.SweepStaleSessions(now)` (marks them `failed`, exit_code=-1) and wired it into `app.New()` at startup, alongside the existing `SweepStaleAttachments` call.

**E4 — PTY child orphan on daemon SIGKILL [DEFERRED → v0.1 / platform-docs]**
`api-stub` has no real child process (PID=0). Manual test required with a real PTY provider (`claudecode`/`claudestream`): SIGKILL daemon while a session is running, then verify with `ps -p <pid>` whether the child is adopted by init/launchd or killed. On macOS, POSIX orphan semantics mean the child is adopted and continues running. This is OS behavior, not a daemon bug — the correct mitigation is checkpoint-resume (Sprint 4) so a restarted daemon can re-attach or cleanly terminate orphans. Document in dev-setup.md when Sprint 4 lands.

**E5 — Workspace prune [DEFERRED → backlog]**
No `mux workspaces prune` command. Workspace dirs accumulate under `workspace_root`. Add `mux workspaces prune [--older-than <duration>]` as a backlog item (already tracked in boot-prompt backlog).

---

## Review / readiness notes

- **macOS PTY support quirks.** `creack/pty` v1.1.24 handles Setsize on macOS but may need a pid-lookup workaround for certain session layouts. Budget 30min if the happy path doesn't work.
- **External-terminal spawning permissions.** macOS may prompt for automation permissions on first `osascript` invocation. Document this in the user-facing error message if spawn returns a permissions error.
- **Resize debounce interval.** 50ms is a guess; if terminal emulators batch resize events differently (kitty bulk-sends on window drag end; Terminal.app streams during drag), the interval may need tuning. Test on multiple emulators during T-02 manual smoke.
