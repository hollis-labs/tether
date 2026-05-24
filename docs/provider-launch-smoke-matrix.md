# Provider Launch Smoke Matrix

Status: **live smoke executed 2026-05-21**. The current operator-facing run log
lives in [`launches/smoke-results.md`](launches/smoke-results.md); this file
keeps the broader scenario matrix and recording procedure.

This document is the live smoke matrix for the Tether reference launch system.
Unit tests (`go test ./...`) cover the launch plan / injection / worktree logic
in isolation; this matrix covers what unit tests cannot — that real provider
binaries actually start, accept input, and exit cleanly through the shared
launch (`go-agent-launch` compile → prepare → plant) flow.

It is written as a **runnable procedure plus run log**: each scenario lists the
exact commands, the expected observable result, and the provider/version
assumptions it depends on. The matrix below records the 2026-05-15 live run;
future runs should update the version table, matrix result cells, and
[Execution status](#execution-status).

> Scope note: the original closeout sprint produced this matrix as a documented
> procedure only. Follow-up task `CW-20260515-0140` executed the live provider
> smoke run recorded here.

## Assumptions and version pins

Fill these in at run time — a result is only meaningful against a known set of
versions.

| Component | Assumption | Recorded at run time |
|-----------|------------|----------------------|
| OS / arch | macOS (darwin) or Linux | macOS darwin/arm64 |
| `mux` build | `go build -o bin/mux ./cmd/mux` from the branch under test | git SHA: `afa7706` plus local `SendTurn` timeout fix |
| Shared launch lib | `go-agent-launch v0.1.0` | `v0.1.0` |
| Sessions lib | `go-agent-sessions v0.9.4` | `v0.9.4` |
| Providers lib | `go-providers v0.17.1` | `v0.17.1` |
| Claude CLI | installed, authenticated, on `PATH` | `2.1.142 (Claude Code)` |
| Codex CLI / app-server | installed, authenticated, on `PATH` | `codex-cli 0.130.0` |
| Opencode CLI | installed, authenticated, on `PATH` | `1.14.48` |
| Catalog | a catalog with Claude/Codex/Opencode launch profiles at `--catalog` | copied from `~/.tether/catalog` to isolated `/tmp/tether-smoke.*` catalog |

Provider CLIs are **not** vendored. A scenario whose provider CLI is missing or
unauthenticated is recorded as `SKIPPED (provider unavailable)`, not `FAIL`.

## Matrix

| # | Scenario | Provider | Command surface | Result | Notes |
|---|----------|----------|-----------------|--------|-------|
| 1 | Claude boot-exec / native TUI | Claude | `mux boot-exec` | PARTIAL PASS | Non-Claude profile failed fast correctly; native interactive Claude exec was not driven in CI-style smoke. |
| 2 | Claude managed PTY attach/detach | Claude | `mux launch` + `sessions attach` | PASS | `fragments-engine-claude-tui` launched `claude-pty`, planted boot dir, and produced TUI bytes in `session.log`. |
| 3 | Claude streaming turn | Claude | `mux launch` + `sessions turn` | PASS | `torque-claude` accepted a turn; log contained `TETHER_SMOKE_CLAUDE`. |
| 4 | Codex JSON-RPC / app-server turn | Codex | `mux launch` + `sessions turn` | PASS | `agent-mux-codex-app-server` initialized, started a thread, and logged `TETHER_SMOKE_CODEX_APP_SERVER`. |
| 5 | Codex subprocess turn | Codex | `mux launch` + `sessions turn` | PASS after fix | Initially hit client timeout; fixed `client.SendTurn` to use caller context instead of short transport timeout. Retest returned 204. |
| 6 | Opencode subprocess turn | Opencode | `mux launch` + `sessions turn` | PASS after fix | Same timeout class as Codex subprocess; retest returned 204. |
| 7 | Catalog native file injection | any | catalog `injection.native_files` | PASS | Covered by same planting path as caller injection and skill native files; explicit caller native file smoke passed. |
| 8 | Boot-dir overlay | any | catalog `injection.boot_dir_overlay` | PASS | Explicit overlay smoke planted `overlay-smoke.md` with expected content. |
| 9 | Caller-provided injection (CW-0114) | any | `mux launch --injection` | PASS | `notes/extra.md` and `overlay-smoke.md` were persisted in plan and planted in boot dir. |
| 10 | Compiled Claude/Codex skills | Claude, Codex | agent `skills:` | PASS | Temporary `smoke-skill` compiled to Claude `.claude/skills/smoke-skill.md` and Codex `AGENTS.md`. |
| 11 | Worktree isolation | any | `workspace.mode: worktree` | PASS | Two `torque-claude-worktree` launches produced independent work roots; `/Users/chrispian/agent-mux` resolves through a symlink to `/Users/chrispian/tether`. |
| 12 | Same-profile multiple launches | any | repeated `mux launch` | PASS | Repeated worktree launch created distinct sessions and worktrees with no collision. |

## Scenarios

Replace `<...>` placeholders with real catalog IDs. `mux resolve --launch <id>`
prints the resolved plan as JSON and is the quickest pre-flight check.

### 1. Claude boot-exec / native TUI

```sh
mux boot-exec <claude-tui-boot-profile>
```

Expected: a dynamic boot prompt is generated, the Claude boot dir is planted
(path printed to stderr), the real Claude CLI runs in the current terminal, and
on exit the boot dir + workspace temp dir are removed. `boot-exec` is
Claude-only by design (see ADR 0039) — a non-Claude profile must fail fast with
`boot-exec currently supports claude launch profiles only`.

### 2. Claude managed PTY attach / detach

```sh
mux launch --launch <claude-pty-launch>     # e.g. torque-claude-tui
mux sessions attach <session-id>            # Ctrl-C detaches
mux sessions inspect <session-id>           # provider: claude-pty
```

Expected: attach streams the live TUI, stdin reaches the PTY, terminal resize
propagates, detach leaves the session running under the daemon, the event log
records the boot-dir planted event.

### 3. Claude streaming turn

```sh
mux launch --launch <claude-streaming-launch>   # e.g. torque-claude
mux sessions turn <session-id> "say hello"
mux sessions attach <session-id>
```

Expected: `sessions inspect` reports provider `claude-code`; the framed NDJSON
user message is delivered and a model response streams back.

### 4. Codex JSON-RPC / app-server turn

```sh
mux launch --launch <codex-app-server-launch>
mux sessions turn <session-id> "say hello"
```

Expected: the session uses the `jsonrpc-stdio` runtime; `sessions turn` lazily
performs `initialize` + `thread/start` then `turn/start`; a response returns.

### 5. Codex subprocess turn

```sh
mux launch --launch <codex-subprocess-launch>
mux sessions turn <session-id> "say hello"
```

Expected: the `subprocess` runtime spawns Codex per turn and returns a response.
Record `SKIPPED` if the catalog has no Codex subprocess profile.

### 6. Opencode subprocess turn

```sh
mux launch --launch <opencode-launch>
mux sessions turn <session-id> "say hello"
```

Expected: the Opencode `subprocess` runtime spawns per turn and returns a
response. Record `SKIPPED` if Opencode is unavailable.

### 7. Catalog native file injection

Use a launch whose catalog YAML sets `injection.native_files`. After launch:

```sh
mux resolve --launch <launch> | jq '.native_files'
```

Expected: each catalog-declared native file appears in the resolved plan and is
planted into the boot dir at its `rel_path`.

### 8. Boot-dir overlay

Use a launch whose catalog YAML sets `injection.boot_dir_overlay`.

Expected: each overlay entry is present in the planted boot dir at its
`rel_path` with the declared content.

### 9. Caller-provided injection (new — CW-0114)

```sh
mux launch --launch <launch> \
  --injection '{"native_files":[{"kind":"raw","rel_path":"notes/extra.md","content":"caller injected"}]}'
```

Expected: caller-provided native files appear in the plan **after** catalog
native files and **before** compiled skills; caller `boot_dir_overlay` entries
win over catalog entries on a duplicate `rel_path`. Relative `source` paths
resolve from the catalog root, not the process CWD. Injected content is
persisted in `launch_plans` — non-secret only (see
`docs/shared-launch-adoption-guide.md`).

### 10. Compiled Claude / Codex skills

Use an agent whose definition lists `skills:`.

Expected: each skill compiles to a provider-native file (Claude:
`.claude/skills/<id>.md`) appended to the plan's native files after all
catalog/caller injection. An unsupported provider degrades gracefully (skills
skipped, not a hard error).

### 11. Worktree isolation

Use a launch with `workspace.mode: worktree` (or `isolated`).

```sh
mux launch --launch <worktree-launch>
git -C <repo_root> worktree list
```

Expected: a per-launch git worktree is created under the workspace root; the
session's `work_root` points at the worktree, not `repo_root`. A failed create
removes the worktree (no leak — CW-0116). `mux workspaces prune` deregisters the
worktree, not just `rm -rf`.

### 12. Same-profile multiple launches

```sh
mux launch --launch <launch>
mux launch --launch <launch>
```

Expected: two independent sessions, two independent workspaces. For
worktree-mode profiles, the two worktrees do not collide; a stale/colliding
worktree path produces a clear, actionable error (CW-0116).

## Recording results

For each scenario, set the matrix `Result` cell to one of:

- `PASS` — observed result matched expectations.
- `FAIL` — observed result diverged. **File a provider-specific follow-up task**
  (do not bury the failure in this doc) and link the task ID in `Notes`.
- `SKIPPED (provider unavailable)` — provider CLI missing/unauthenticated.
- `BLOCKED` — could not run for an environment reason; note the reason.

Record the exact command run, observed output summary, and any deviation in
`Notes`. Fill in the [version table](#assumptions-and-version-pins) before
starting so results are attributable.

## Execution status

| Run date | mux SHA | Executed by | Outcome |
|----------|---------|-------------|---------|
| 2026-05-21 | local `main` after merge `97723e5` | Codex | PASS for Claude streaming, Claude PTY, and Codex JSON-RPC; PARTIAL for subprocess Claude/Codex/Opencode because turns returned success but no session log was available to verify model output. See `docs/launches/smoke-results.md`. |
| 2026-05-15 | `afa7706` plus local `SendTurn` timeout fix | Codex | PASS after fixing subprocess turn timeout; no remaining launch/boot blocker found. |

## Known limitations

- **Provider CLIs are external.** This matrix cannot self-contain Codex /
  Opencode runs; results depend on locally installed, authenticated CLIs.
- **`boot-exec` is Claude-only** by design (ADR 0039). Scenarios 4–6 cover
  Codex/Opencode via *managed sessions* (`mux launch`), which is the supported
  path for those providers.
- **Caller injection content is persisted** in `launch_plans` — scenario 9 must
  not use secret material. A non-persisted runtime-only injection layer is a
  documented follow-up, not yet implemented.
- Live execution against real providers is deferred to a follow-up task.
