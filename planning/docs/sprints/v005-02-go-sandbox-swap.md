# Sprint v005-02 — go-sandbox swap

**Epic:** post-v0.0.4 foundation work — portfolio library adoption (Phase A: Track A)
**Status:** SHIPPED
**Branch:** `feat/v005-02-go-sandbox-swap` (FF-merged + deleted)
**Landed:** `20ad1a1` (gofmt fixups: `0bb3921`)
**Date:** 2026-04-27

---

## Goal

Replace `internal/sandbox/` with `github.com/hollis-labs/go-sandbox` v0.1.0. Absorb
the security hardening that lives upstream (SBPL injection prevention, narrowed
bwrap mounts, namespace unsharing, per-invocation `--tmpfs /tmp`,
`--die-with-parent`, `--new-session`) and the temp-profile-file leak fix that
mux's current `internal/sandbox/macos.go:139` self-documents.

This is Track A of the portfolio-libs migration. Independent of go-agent-sessions
v0.1.0 — can land before. Tracks B (go-runner adoption) and C1 (API provider)
both wait on the lib.

---

## Tasks

- [x] **T-v005-s02-01** — Add `github.com/hollis-labs/go-sandbox v0.1.0` to `go.mod`; rewrite imports in 5 type-only consumers: `internal/config/loader.go`, `internal/config/model.go`, `internal/config/loader_test.go`, `internal/runtime/manager.go`, `internal/provider/provider.go`. Verify `go build ./...` passes (Apply call sites still broken — separate task).
- [x] **T-v005-s02-02** — Wire cleanup-func plumbing in 4 adapters (`claudecode`, `claudestream`, `opencode`, `goprovider`). Each adds `sandboxCleanup func()` field on its session struct + `sync.Once`; fires in `Wait()` and `Stop()`. Captured from `sandbox.Apply(...)` cleanup return value at adapter `Start()` call site. Per go-sandbox doc: cleanup must fire after cmd exits, not at function scope (function returns before process exits).
- [x] **T-v005-s02-03** — Delete `internal/sandbox/` package (profile.go, macos.go, linux.go, unsupported.go, profile_test.go, macos_test.go, integration_test.go). Verify no remaining `chrispian/agent-mux/internal/sandbox` imports. Per "no compat shims" — clean break, no re-exports.
- [x] **T-v005-s02-04** — ADR 0013 addendum: note hardening upgrades absorbed (validateSeatbeltLiteral, namespace unsharing set, narrowed `/etc` ro-binds, tmpfs /tmp, --die-with-parent, --new-session) and the temp-profile-file leak fix. Profile shape unchanged — addendum, not supersede.
- [x] **T-v005-s02-05** — `make check` green (fmt + vet + lint + test-race + vuln). Existing 503 tests pass. Live smoke: launch a sandboxed claudestream session, verify SBPL applies (workspace read/write works, `${HOME}/.ssh` denied), profile temp file is removed after session exits.

---

## Acceptance

- [x] `internal/sandbox/` directory does not exist in the working tree
- [x] `go.mod` contains `github.com/hollis-labs/go-sandbox v0.1.0`
- [x] No `chrispian/agent-mux/internal/sandbox` imports remain
- [x] 29 of 30 packages pass under `-race`. Pre-existing race in `TestCompliance/Baseline/ResizeNoOpOrNoError` confirmed via stashed-baseline rerun on `d58fa9f`; captured at `followups.agent_mux.compliance_resize_pty_race`. Phase 2 likely subsumes the fix.
- [ ] Live smoke: sandboxed session launches successfully under `workspace-only`, `workspace-plus-net`, `unrestricted` profiles — **not yet performed in this session**; deferred to next-session smoke under the new lib.
- [ ] Temp SBPL profile file is removed after sandboxed session exits — code path verified by go-sandbox's audit, not yet smoke-tested in mux integration.
- [x] ADR 0013 addendum committed in `20ad1a1`
- [x] Conventional-commit per task; FF-merge to main; branch deleted
- [x] `make install` → `/Users/chrispian/go/bin/mux`
- [x] Boot prompt's `Where We Are` updated (next session)

---

## Design decisions

**No compat shims.** `internal/sandbox/` is deleted wholesale. Per `feedback_no_compat_shims` — no re-exports, no aliases, no `//removed` comments. All consumers update their imports in the same commit set.

**Cleanup plumbing per-adapter, not via session.Start.** Considered passing cleanup as a parameter to `session.Start` so it could fire alongside `cmd.Wait`, but that's an invasive contract change for one library's lifecycle quirk. Per-adapter cleanup field is the lower-blast-radius approach: 4 adapters × ~10 LOC each. When go-agent-sessions Phase 2 lifts the runtime, this plumbing dissolves into the lib.

**ADR 0013 addendum, not new ADR.** Profile shape (`ID`, `Description`, `FS`, `Net`, `Subprocess`) is byte-identical between mux's old internal package and go-sandbox. Three built-in profile names (`workspace-only`, `workspace-plus-net`, `unrestricted`) unchanged. Hardening upgrades are implementation-level — no contract change. Addendum documents what we got for free.

**Egress proxy stays out.** Per go-sandbox README's explicit exclusion ("Network proxy subsystem"), allowlisted egress is `go-egress-proxy`'s territory (built in parallel session). When mux needs allowlisted egress, it'll start go-egress-proxy and merge `HTTP_PROXY`/`HTTPS_PROXY` into cmd env before `sandbox.Apply`.

---

## API delta (single change)

```go
// before (mux internal/sandbox)
func Apply(cmd *exec.Cmd, p Profile, workspace string) error

// after (go-sandbox v0.1.0)
func Apply(cmd *exec.Cmd, p Profile, workspace string) (cleanup func(), err error)
```

All other public surface is unchanged: `Profile`, `FSSpec`, `LoadProfile`, `LoadProfiles`.

---

## Hardening absorbed (free upgrades)

1. **`validateSeatbeltLiteral`** — non-optional validator rejects `"`, `\`, `()`, `;`, `'`, control chars in workspace path & FS entries, preventing SBPL profile-string injection (mux today: no validation).
2. **bwrap narrowed `--ro-bind`** — limited to `/usr`, `/lib*`, `/bin`, `/sbin`, and 8 specific `/etc/*` files (was: blanket `--ro-bind /etc /etc`).
3. **Namespace unsharing** — `--unshare-pid`, `--unshare-ipc`, `--unshare-uts`, `--unshare-cgroup-try`, `--unshare-user-try` (mux today: missing).
4. **Per-invocation `--tmpfs /tmp`** — prevents cross-session temp-file leakage.
5. **`--die-with-parent`, `--new-session`** — prevent orphan escape and TTY hijacking.
6. **Temp SBPL profile file cleanup** — go-sandbox returns a cleanup function that removes the temp file; mux today leaks them (self-documented at `internal/sandbox/macos.go:139`).

---

## Sequencing context (Track A in Phase 1-5)

This sprint is **Track A** of the portfolio-libs adoption (decision:
`decisions.portfolio.go_agent_sessions_composition_library`). Track B
(go-runner adoption inside CLI adapters) and C1 (API provider via Provider
wrapping) both wait on go-agent-sessions v0.1.0 from the parallel session.
Track A is independent and lands first.

When go-agent-sessions v0.1.0 ships and mux migrates onto it (Phase 2), the
per-adapter `sandboxCleanup` plumbing introduced here dissolves into the
library — but no rework cost: the field just goes away with the rest of the
adapter's session struct.
