# Launch Setup Guide

Tether launches are catalog records. A launch profile selects a project, agent,
provider, workspace mode, and prompt inputs; the provider record selects the
runtime transport.

Before launching, preflight the resolved plan:

```sh
mux resolve --launch <launch-id>
```

Then start the session:

```sh
mux launch --launch <launch-id>
```

For long-lived turn-based sessions, prefer:

```sh
mux sessions turn <session-id> "your message"
```

Use `mux sessions input` for raw PTY/TUI sessions.

## Runtime Modes

| Mode | Providers | Best for | Notes |
|---|---|---|---|
| `streaming-stdio` | Claude | Managed Claude Code sessions driven by framed turns. | Recommended Claude automation path. |
| `jsonrpc-stdio` | Codex | Managed Codex app-server sessions driven by JSON-RPC. | Recommended Codex session path. |
| `pty` | Claude | Human-operated Claude TUI sessions. | Raw terminal bytes; use carriage return to submit input. |
| `subprocess` | Claude, Codex, Opencode | Compatibility launches that spawn provider CLI work per turn. | Current CLI smoke confirms turns are accepted, but output/log observability is limited. |
| `api` | API stub | Tests and no-op development flows. | Not a real agent CLI. |

## Claude Launches

### Managed Streaming

Use this for daemon-owned Claude Code sessions that accept `sessions turn`, MCP
`mux_session_send_turn`, or HTTP send-turn requests.

Provider example:
[`examples/catalog/providers/claude-code.yaml`](../../examples/catalog/providers/claude-code.yaml)

Launch example:
[`examples/catalog/launches/torque-claude.yaml`](../../examples/catalog/launches/torque-claude.yaml)

```yaml
id: my-claude
project: my-project
agent: my-agent
provider: claude-code
workspace:
  mode: hybrid
prompt:
  include_project_boot: true
  include_agent_boot: true
```

Smoke status: PASS on 2026-05-21. The session launched, accepted a turn, and
returned `TETHER_CLAUDE_STREAMING_OK`.

Limitation: provider authentication and rate limits are external to Tether. The
smoke run observed a Claude `rate_limit_event` while still completing
successfully.

### PTY / TUI

Use this when a human needs the native Claude terminal UI.

Provider example:
[`examples/catalog/providers/claude-pty.yaml`](../../examples/catalog/providers/claude-pty.yaml)

Launch example:
[`examples/catalog/launches/torque-claude-tui.yaml`](../../examples/catalog/launches/torque-claude-tui.yaml)

```sh
mux launch --launch torque-claude-tui
mux sessions attach <session-id>
```

Smoke status: PASS on 2026-05-21. Raw input reached the TUI and Claude returned
`TETHER_CLAUDE_PTY_OK`.

Limitations: this is a terminal UI path, not a structured automation path.
Logs contain terminal control sequences, and `sessions input` may need `\r` to
submit the typed prompt. Stopping a PTY session can report a signal exit code.

### Subprocess Compatibility

Use this only when you need the legacy go-provider subprocess adapter.

Provider example:
[`examples/catalog/providers/claude-goprovider.yaml`](../../examples/catalog/providers/claude-goprovider.yaml)

Launch example:
[`examples/catalog/launches/claude-gp-launch.yaml`](../../examples/catalog/launches/claude-gp-launch.yaml)

Smoke status: PARTIAL on 2026-05-21. The session launched and `sessions turn`
returned success, but no session log was available to verify model output.

Limitation: subprocess launches currently have weaker output observability than
`streaming-stdio` and `jsonrpc-stdio`.

## Codex Launches

### JSON-RPC App Server

Use this for daemon-owned Codex sessions. Tether starts `codex app-server` and
sends JSON-RPC `initialize`, `thread/start`, and `turn/start` calls over stdio.

Provider example:
[`examples/catalog/providers/codex-app-server.yaml`](../../examples/catalog/providers/codex-app-server.yaml)

Launch example:
[`examples/catalog/launches/agent-mux-codex-app-server.yaml`](../../examples/catalog/launches/agent-mux-codex-app-server.yaml)

Smoke status: PASS on 2026-05-21. The same session accepted two turns and
returned `TETHER_CODEX_JSONRPC_OK` and `TETHER_CODEX_SECOND_OK` on one Codex
thread.

Limitation: this depends on a Codex CLI with `app-server` support and local
authentication.

### Subprocess Compatibility

Use this for the legacy Codex CLI adapter path.

Provider example:
[`examples/catalog/providers/codex-cli.yaml`](../../examples/catalog/providers/codex-cli.yaml)

Launch example:
[`examples/catalog/launches/agent-mux-codex-launch.yaml`](../../examples/catalog/launches/agent-mux-codex-launch.yaml)

Smoke status: PARTIAL on 2026-05-21. The session launched and `sessions turn`
returned success, but no session log was available to verify model output.

Limitation: prefer `codex-app-server` for session continuity and observable
turn output.

## Opencode Launches

Opencode currently uses the subprocess adapter.

Provider example:
[`examples/catalog/providers/opencode.yaml`](../../examples/catalog/providers/opencode.yaml)

Launch example:
[`examples/catalog/launches/agent-mux-opencode.yaml`](../../examples/catalog/launches/agent-mux-opencode.yaml)

Smoke status: PARTIAL on 2026-05-21. The session launched and `sessions turn`
returned success, but no session log was available to verify model output.

Limitations: no PTY or JSON-RPC Opencode runtime is currently wired in Tether.
Output observability is the same subprocess limitation described above.

## Workspace Modes

| Mode | Behavior |
|---|---|
| `hybrid` | Run provider work in the project `repo_root`, while keeping Tether session files in a session workspace. |
| `shared` | Alias for shared `repo_root` behavior. |
| `worktree` | Create a per-session git worktree and run the provider there. |
| `isolated` | Alias for `worktree`. |

Use `hybrid` for local development sessions that should work directly in the
checkout. Use `worktree` when several agents need independent working trees.

## Quick Smoke

For each launch:

```sh
mux resolve --launch <launch-id>
mux launch --launch <launch-id>
mux sessions turn <session-id> "Smoke test: reply with exactly TETHER_SMOKE_OK and nothing else."
mux sessions stop <session-id>
```

For PTY launches:

```sh
mux launch --launch <claude-pty-launch>
mux sessions input <session-id> $'Smoke test: reply with exactly TETHER_SMOKE_OK and nothing else.\r'
mux sessions tail <session-id> --follow=false
mux sessions stop <session-id>
```

Record provider versions, session IDs, and any limitations in
[smoke-results.md](smoke-results.md).
