# Sprint v002-01 — Daemon Runtime

Epic: [v0.0.2](../epics/v0.0.2-runtime-foundation.md)

**Epic:** [v0.0.2](../epics/v0.0.2-runtime-foundation.md)
**Goal:** Introduce a long-lived local process that owns running sessions across CLI invocations. Replace the one-shot `app.Service` lifetime with a daemon that new CLI commands talk to. Lock down shared-state concurrency so multiple clients and background goroutines cannot corrupt the registry of active handles.
**Exit criteria:**
- [x] A daemon entrypoint exists — either `cmd/muxd` or `mux daemon start/stop/status` — that runs as a long-lived process. *(`mux daemon start/run/stop/status` wired in T-02.)*
- [x] Running sessions survive the invoking CLI's exit and are visible to later `mux sessions list` calls from fresh processes. *(T-03 smoke: launched sleep session, confirmed visible from fresh invocation until manually stopped.)*
- [x] The daemon writes a PID file and opens a local socket (or HTTP listener) that the CLI uses to talk to it. *(unix socket by default per ADR 0002; PID file under ~/.agent-mux/run/ by default; CLI uses `internal/client` to reach both.)*
- [x] The runtime's registry of active handles is guarded by a mutex; no raced writes to the `running`/`killing` maps under `go test -race`. *(T-01 introduced the sync.RWMutex; T-04 added the adversarial interleaved test and sanity-checked the mutex is load-bearing.)*
- [x] Graceful shutdown: daemon stops accepting new launches, waits (up to a configurable timeout) for in-flight sessions to reach a terminal state, persists, then exits. *(`Manager.Shutdown` + `daemon.Server.Run` coordinated pipeline; `shutdown_timeout` in global.yaml.)*
- [x] Signal handling: SIGINT/SIGTERM trigger graceful shutdown; SIGHUP is reserved for future reload (noop is acceptable in v0.0.2). *(`signal.NotifyContext(ctx, SIGINT, SIGTERM)` in `daemon run`; SIGHUP untouched = default ignore, acceptable.)*

## Context

Context-pack §06 enumerates the specific v0 limitations this sprint closes: one-shot CLI runtime, snapshot-only ownership of handles, stop-only-on-own-process, and unsynchronized maps. Context-pack §07 proposes the `internal/runtime` package as the home for daemon/runtime ownership: "registry of active handles, lifecycle transitions, attach manager, input injection, stop/restart, checkpoint trigger coordination." Commit `d2383c0` already flags the unsynchronized map issue as known technical debt.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/app/service.go` — `Service.running` map, no locking; `Launch` / `StopSession` mutate from multiple goroutines.
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/root.go` — persistent flag for catalog path; needs a new subcommand tree (or sibling binary) for daemon mode.
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/launch.go`, `sessions.go` — today these build a fresh `Service` each invocation; post-sprint they must route through the daemon for launch/list/stop.

When done, the daemon is the single runtime owner and the CLI is a thin client that opens a connection, issues a request, and exits. The local API surface (Sprint v002-05) will plug into the same daemon process.

## Tasks

### T-v002-s01-01: Introduce `internal/runtime` package owning active handles

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, daemon, runtime, foundation]

#### Problem

Today `app.Service` owns `running map[string]*session.Handle` directly. There's no dedicated runtime package, no lifecycle manager, and `app.Service` is on track to become a god-package.

#### Evidence

`internal/app/service.go` holds the handle map, wires config+store, and drives lifecycle transitions. Commit `d2383c0` documented that `running`/`killing` access is not synchronized. Context-pack §07 calls out `internal/runtime` as a target package specifically to avoid this concentration in `app`.

#### Fix direction

Create `internal/runtime/manager.go`:
- `Manager` struct with: registry (id → handle), `sync.RWMutex`, event sink, store reference.
- Methods: `Start(ctx, plan, workspace) (sessionID, error)`, `Stop(ctx, id) error`, `Get(id) (*Handle, bool)`, `List() []SessionSnapshot`, `Shutdown(ctx) error`.
- Move lifecycle goroutine (wait for exit → update state → delete from map) here from `app.Service`.
- Leave `app.Service` as a thin composition root that holds a `*runtime.Manager`.

Do not expose raw `*session.Handle` from `Manager.Get`; return an interface or a handle-token so future packages (attach manager, input injector) don't directly reach into PTY internals.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/` (new package)
- `/Users/chrispian/Projects-apps/agent-mux/internal/app/service.go` — slim down: delegate lifecycle to `runtime.Manager`
- `/Users/chrispian/Projects-apps/agent-mux/internal/session/runtime.go` — may need a small interface extraction so the manager can consume it

#### Acceptance criteria

- [x] `internal/runtime/manager.go` exists and owns the session handle registry.
- [x] `app.Service` no longer references `session.Handle` directly for launch/stop/list.
- [x] All registry access goes through a method that takes/releases the mutex.
- [x] `go test -race ./...` passes.
- [x] Existing CLI smoke path (launch → sessions list → sessions get) still works. *(build + read-only CLI verified; live `mux launch` against a real provider gated on Sprint 4 env-merge fix per boot-prompt critical-state note #5.)*

#### Test plan

- Unit tests in `internal/runtime/` covering: start → list, start → stop, concurrent start/stop under `-race`. *(15 tests, all green under -race.)*
- Integration-style test that launches a stub command (`/bin/sleep 0.1` via a test provider) and verifies state transitions. *(`TestManager_RealPTYLifecycle` exercises the DefaultStarter with `/bin/echo`.)*

#### Scope fences

- Do not introduce the daemon process in this task — this is just the refactor. Daemon wiring is T-v002-s01-02.
- Do not add attach/input injection here — that's Sprint v002-02.
- Do not rewrite `session.Handle` internals. Keep PTY logic in `internal/session/`.
- Do not add the local HTTP API here — that's Sprint v002-05.

#### Relationship

Blocks: T-v002-s01-02, T-v002-s01-03, T-v002-s02-01 (attach manager), T-v002-s05-01 (API handlers).
Siblings: none.

#### Origin

Context-pack [07-proposed-package-boundaries.md](../agent-mux-vfuture-context-pack/07-proposed-package-boundaries.md), [06-review-of-current-v0.md](../agent-mux-vfuture-context-pack/06-review-of-current-v0.md) §5.

---

### T-v002-s01-02: Add daemon entrypoint with PID file + socket

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, daemon, cli]

#### Problem

There is no long-lived process. Each `mux …` command builds a fresh `app.Service`, runs, and exits. Sessions launched with `mux launch --wait` keep the process alive, but anything launched without `--wait` either blocks the CLI or is abandoned.

#### Fix direction

Choose one of:
- **Option A (preferred):** add `mux daemon start|stop|status` subcommands in `cmd/mux/daemon.go`, reuse the same binary. Simpler to distribute; fewer build artifacts.
- **Option B:** add a separate `cmd/muxd/` entrypoint. Cleaner conceptual split; more consistent with context-pack §07 ("cmd/muxd (optional dedicated daemon entrypoint)").

Recommend Option A for v0.0.2 with a note that Option B is trivial to add later by symlinking or stripping root.

Implementation:
- On `daemon start`: fork-or-exec self with a marker flag, parent exits after child writes PID file + opens listener.
- PID file at `~/.agent-mux/run/muxd.pid` (or `$XDG_RUNTIME_DIR/agent-mux/muxd.pid` if set).
- Listener: loopback HTTP on a port from config (default `127.0.0.1:7180`) AND/OR a Unix socket at `~/.agent-mux/run/muxd.sock`. Transport choice is a readiness-review decision; scaffold for both.
- On `daemon stop`: read PID, send SIGTERM, wait, remove PID file.
- On `daemon status`: print PID, uptime, listener address, active session count.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/daemon.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/root.go` — register `daemonCmd`
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` — add `Shutdown(ctx)` hook the daemon calls on signal
- `/Users/chrispian/Projects-apps/agent-mux/examples/catalog/global.yaml` — add `daemon:` config block (listen address, PID path)

#### Acceptance criteria

- [x] `mux daemon start` returns quickly; a child process is visible in `ps`; PID file exists. *(Smoke confirmed: start returned with pid/listener/pidfile output; pgrep found the child; /tmp/mux-smoke/run/muxd.pid + muxd.sock both present.)*
- [x] `mux daemon status` prints the expected fields. *(pid / uptime / listener / sessions — hits /health via the DialHTTPClient helper.)*
- [x] `mux daemon stop` terminates the child and removes the PID file. *(SIGTERM → Server.Run drains Manager + removes both PID file and unix socket.)*
- [x] Trying to start when already running is a clear error, not a silent second daemon. *(Pre-flight in both `daemon start` and `Server.Run` returns `daemon already running (pid X, pidfile Y)`.)*
- [x] SIGINT/SIGTERM to the daemon trigger `Manager.Shutdown`. *(`signal.NotifyContext` feeds the ctx Run blocks on; ctx cancel triggers http.Server.Shutdown → Manager.Shutdown → store.Close sequence. Verified by `TestServer_RunServesHealthAndShutsDown` and manual `daemon stop` smoke.)*

#### Test plan

- Manual: `mux daemon start && mux daemon status && mux daemon stop` sequence; verify PID file lifecycle. *(Done with ephemeral catalog under /tmp/mux-smoke/; PID 2829, clean teardown.)*
- Integration test: spawn daemon in a subtest, connect, issue a health ping, stop it. *(`TestServer_RunServesHealthAndShutsDown` + `TestServer_RunRefusesSecondInstance` in `internal/daemon/daemon_test.go`.)*
- Shutdown test: start a fake session via the stubbed provider, send SIGTERM, verify session reaches terminal state and is persisted. *(Deferred — stubbed provider arrives in Sprint v002-s04 T-01; Sprint 1 smoke instead exercises the /health endpoint + shutdown path without a live session.)*

#### Scope fences

- Do not implement the full HTTP API surface here — stub a single `/health` endpoint to prove the listener. Real endpoints land in Sprint v002-05.
- Do not implement supervisor/restart logic — that's Cerberus's job later (v0.3+).
- Do not add systemd / launchd plists here — deferred to v0.3+.

#### Relationship

Depends on: T-v002-s01-01.
Blocks: T-v002-s01-03, T-v002-s05-01.

#### Origin

Context-pack [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md) (v0.0.2 "long-lived local daemon/runtime"), [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) (runtime lifecycle section).

---

### T-v002-s01-03: Route CLI commands through the daemon

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, cli, daemon]

#### Problem

Existing `mux launch`, `mux sessions list|get|stop`, `mux sessions tail` bypass any daemon — they build a fresh `app.Service` and talk to SQLite / PTY directly. Once a daemon exists, these commands must route through it to see running sessions correctly.

#### Evidence

- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/launch.go` calls `app.New(catalogPath)` and launches the PTY in-process.
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/sessions.go` builds a fresh `Service` to hit SQLite.
- The result: a detached session started via `mux launch` is owned by a dead shell process, so `mux sessions stop <id>` from a fresh invocation can't actually signal it.

#### Fix direction

Introduce a thin client in `internal/client/` (or similar) that calls the daemon's local API (stubbed in v002-s01-02, filled in during Sprint v002-05).

Command migration:
- `mux launch` → POST to daemon's launch endpoint, stream the returned session ID.
- `mux sessions list|get|stop` → talk to daemon.
- `mux sessions tail` → talk to daemon's attach stream (stub for now; real live-follow is Sprint v002-02).

Behavior if daemon not running:
- For `launch` and `stop`: clear error: "agent-mux daemon is not running; run `mux daemon start`".
- For `sessions list|get`: fall back to read-only SQLite (historical sessions). Document this behavior.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/client/` (new; thin HTTP/socket client)
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/launch.go`
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/sessions.go`
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/resolve.go` — resolve stays local (it's a pure read of the catalog)

#### Acceptance criteria

- [x] With daemon running: `mux launch --launch demo-launch` returns quickly with a session ID; the session is visible from a fresh `mux sessions list`. *(Smoke with `echo-launch` at /tmp/mux-smoke/: launch returned session 7c495eba…, list showed `completed` immediately after --wait returned exit 0.)*
- [x] With daemon not running: `mux launch` errors cleanly; `mux sessions list` shows historical rows from SQLite. *(Clean error text: "agent-mux daemon is not running; run `mux daemon start` first". list fell back to store and returned both prior rows.)*
- [x] `mux sessions stop <id>` actually terminates the daemon-owned process. *(Launched `/bin/sleep 30`, stop returned, state=killed, pgrep confirmed no lingering sleep process.)*
- [x] No code path in `cmd/mux/` still constructs `*session.Handle` directly. *(`grep -r session\\.Handle cmd/` returns no matches.)*

#### Test plan

- Smoke: start daemon, launch, list, stop; verify PID is gone. *(Covered in smoke run above.)*
- Fallback: with daemon down, `mux sessions list` returns historical rows without erroring. *(Covered.)*
- Error path: `mux launch` without daemon returns the clean error, not a stack trace. *(Covered — stack suppressed; cobra still prints Usage block which is expected default.)*
- Client package tests: 8 tests in `internal/client/` exercising the full RPC surface against a real `httptest.Server` wired to the daemon handlers, plus unreachable-path coverage for both unix and tcp transports.
- Handler tests: 14 tests in `internal/daemon/handlers_test.go` (launch / list / get / stop / wait / routing / method-not-allowed / no-service guard).

#### Scope fences

- Do not implement the full local API here (stubs are fine for endpoints filled in by v002-s05). Real attach/checkpoint/broker endpoints are later sprints.
- Do not introduce a new CLI framework — stay on Cobra.
- Do not break `mux resolve` (it can stay fully local — no daemon needed).

#### Relationship

Depends on: T-v002-s01-02.
Overlaps with: T-v002-s05-01 (client will fill in as API surface grows).

#### Origin

Context-pack [06-review-of-current-v0.md](../agent-mux-vfuture-context-pack/06-review-of-current-v0.md) §1, [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md).

---

### T-v002-s01-04: Add `-race` to CI + smoke tests for concurrent lifecycle

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [quality, testing, concurrency]

#### Problem

The v0.0.1 known-debt item (commit `d2383c0`) is invisible without a race detector in CI. Until there's a test that actually races launch vs stop vs waitgroup completion, future edits to `runtime.Manager` can reintroduce the bug.

#### Fix direction

- Add `go test -race ./...` to the Makefile `test` target (or split as `test-race`).
- Add an integration test in `internal/runtime/manager_test.go` that: starts N stub sessions concurrently, stops a subset mid-flight, shuts down, asserts all ended up in terminal states with no panics / no leaked goroutines.
- Optional: add `go.uber.org/goleak` or similar to verify no goroutine leaks at `Manager.Shutdown`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/Makefile`
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager_test.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/.github/workflows/` (if/when CI is introduced by Sprint v002-07)

#### Acceptance criteria

- [x] `make test-race` passes. *(Makefile gained `test-race` + `check` targets; full suite green.)*
- [x] The concurrent lifecycle test fails if the manager's mutex is removed (sanity check). *(Confirmed by commenting out the lock block in `Manager.Start`: `-race` reported a data race on `m.registry` + the runtime panicked with "concurrent map iteration and map write" from the new `TestManager_InterleavedLifecycleNoRace` reader goroutines. Restored immediately after.)*
- [ ] CI (once added in v002-07) runs tests with `-race`. *(Deferred to Sprint v002-07 as designed — T-04 just exposes the `test-race` target Sprint 7 will wire into the workflow file.)*

#### Test plan

Self-evidencing — the test is the deliverable. Verified by temporarily removing the mutex in `Start` and watching the race detector fire at line 172. `TestManager_InterleavedLifecycleNoRace` uses a start/stop/read/complete barrier with 120 goroutines (N=30 × 4 roles) hitting the same ID set so Start, Stop, Get, List, and watch-goroutine deletes all overlap.

#### Scope fences

- Do not chase every hypothetical race. Focus on launch/stop/shutdown.
- Do not add `goleak` if it's excessive — the manual shutdown assertion is enough.

#### Relationship

Depends on: T-v002-s01-01.
Pairs with: T-v002-s07-01 (CI integration).

#### Origin

Commit `d2383c0` in the v0.0.1 git log; [06-review-of-current-v0.md](../agent-mux-vfuture-context-pack/06-review-of-current-v0.md) §5.

## Review / readiness notes

- **Transport decision (HTTP vs Unix socket) is deferred to Sprint v002-05.** This sprint scaffolds both; the choice can be made late.
- **Daemon supervision:** out of scope. v0.3+ with Cerberus. For now, users start the daemon manually.
- **Windows support:** not addressed. Unix-only assumptions (SIGTERM, Unix socket, PTY) are acceptable for v0.0.2. Flag as a readiness gap if Windows support becomes a target.
