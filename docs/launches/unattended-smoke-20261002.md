# Unattended provider acceptance — 2026-10-02

Task: CW-20261001-0251. Candidate: `task/CW-20261001-0251-unattended`,
based on `761cf29`. Host: agent-os, Linux amd64. Providers: codex-cli 0.159.3,
agy 1.2.14. Shared dependencies: agentkit v0.21.0, go-providers v0.42.0.

## Isolation and procedure

Built `go build -o "$TMPDIR/tether-0251" ./cmd/tether` in the task worktree.
Ran a separate `tether --catalog "$TMPDIR/live-0251/catalog" daemon run` with
HOME at `$TMPDIR/live-0251/home`, its own Unix socket, DB, workspace and catalog.
The disposable repository contained README.md and a two-line Python add function;
it had a local `.git` boundary. No live Tether restart or live catalog edit.
Only existing credential files were linked into the temporary HOME: Codex
`auth.json` and agy's `antigravity-oauth-token`; credentials were not printed or
copied into the catalog or this record. The rest of provider state was temporary.

The temporary agent explicitly requested `permissions.permission_mode: bypass`.
Codex's planted config and launch argv used `danger-full-access` / `never`;
agy used `--dangerously-skip-permissions`. Upstream MCP grants were an empty
list; the planted local Tether tools targeted this isolated daemon.

Created sessions with POST `/sessions`, started with POST `/sessions/{id}/launch`,
and delivered prompts with POST `/sessions/{id}/turn`. Notification probes used
POST `/messages/notify`, with a `msg://session/local/<session-id>` recipient and
an explicit `wake_text`. Stopped every created live session via the same daemon.

## Results

| Check | Codex app-server | Antigravity subprocess |
|---|---|---|
| Read-only summary | PASS: read README.md/sum.py and described integer addition | PASS: read both files and described integer addition |
| MCP | PASS: actual `tether_session_list` call completed, `ok: true` | PASS: actual MCP call returned `ok: true` and isolated sessions |
| Second turn | PASS: replied `TETHER0251`, same thread | PASS: replied `TETHER0251`, same conversation mapping |
| Correction sent during sleep 15 | PASS: active turn's final reply became `STEERED0251` | PARTIAL: correction waited for running subprocess; first reply `ORIGINAL0251`, next reply `STEERED0251` |
| Notification | PASS: `wake_attempted: true`, `wake_delivered: true`, reply `NOTIFIED0251` | PASS: same response flags and reply `NOTIFIED0251` |
| Idle stop | PASS: terminal `killed`, PID 0 | PASS: terminal `killed`, PID 0 |
| Stop during sleep 60 | PASS: stopped before turn completed, terminal `killed`, process gone | PASS: in-flight turn returned `turn_failed` / signal 15, terminal `killed`, process gone |

Codex main session: `4754ad60-9db8-4ae2-af87-a9893d878859`;
thread `01a0fcfc-2a2f-7713-855f-b47fc0bea2c5` persisted across summary,
recall, steering, and notification. Its tool completion record had status
`completed`, tool `tether_session_list`, server `tether`, and no error.

Antigravity main session: `867cbcfa-1f14-4e3f-923f-0abe14a852e3`;
conversation `3d4318b0-268e-4c7d-a341-de7ec096e13b`. The read-only
`session_provider_mappings` query returned this same ID before and after turn two.
Its log recorded `[tool_use:call_mcp_tool]` and the successful session list.

Active cancellation sessions: Codex `479963f6-8313-4af2-bb56-09f329dc316a`,
agy `b1e409b3-b168-448f-b626-727810d57d2d`. Antigravity's terminal DTO retained
a stale PID value after the process exited; the state was `killed` and that PID
no longer existed. This record does not treat a populated PID as evidence of
an active process.

## Limits and fixes

The first Codex attempts exited 2 because the legacy catalog supplied
`app-server` and the shared argv projection supplied it again. This candidate
strips that legacy prefix and corrects the example provider. The initial agy
probe lacked the credential link and entered sign-in; it was stopped and retried
with the existing token linked into the temporary HOME.

Explicit Codex bypass previously mapped to accept-edits. This candidate maps
it to registry yolo, producing full-access/never in argv and planted config.
The earlier assumption that approval `never` always rejects MCP does not apply
to full-access on Codex 0.159.3; the successful live call above verifies it.
Default sessions retain workspace-write/on-request and the existing MCP hook.

Active-turn agy interruption remains a shared-runtime gap, filed as
CW-20261002-0078 under EP-20260930-0001. Input submitted while busy is preserved
and acted on in a subsequent turn; it does not change the current turn. The
current registry supports agy only as subprocess-per-turn. This run does not
claim full active-turn steering acceptance for agy, cold daemon checkpoint
resume, or Codex subprocess live acceptance. Unit tests cover the Codex
subprocess bypass argv and planted policy without invoking a real model CLI.
