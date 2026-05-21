# Launch Smoke Results

Status: live smoke executed 2026-05-21 from local catalog profiles.

## Versions

| Component | Version / state |
|---|---|
| Claude CLI | `2.1.146 (Claude Code)` |
| Codex CLI | `codex-cli 0.132.0` |
| Opencode CLI | `1.15.6` |
| Tether daemon | running from locally installed `mux` after `cerberus resource deploy tether-daemon-service` |

## Results

| Launch | Provider/runtime | Session | Result | Notes |
|---|---|---|---|---|
| `agent-mux-claude` | Claude `streaming-stdio` | `56a21a4e-6f50-4901-9838-f13503c30dc2` | PASS | Returned `TETHER_CLAUDE_STREAMING_OK`. Claude also emitted a rate-limit event, but the turn completed. |
| `chrispian-claude-tui` | Claude `pty` | `7de2aa55-99fa-4c72-bb92-70368048bf0f` | PASS | Raw PTY input plus carriage return returned `TETHER_CLAUDE_PTY_OK`. Stop ended with signal-style exit code. |
| `agent-mux-claude-stream` | Claude `subprocess` | `ac38e031-18a3-4e67-adc4-499183021e9e` | PARTIAL | Launch and turn returned success; no `logs/session.log` was available to verify model output. |
| `agent-mux-codex-app-server` | Codex `jsonrpc-stdio` | `2b8db2b9-b241-438e-883d-ec4f765ab406` | PASS | Two turns returned `TETHER_CODEX_JSONRPC_OK` and `TETHER_CODEX_SECOND_OK` on thread `019e4c66-566f-7cd1-9617-dd087c5da2c9`. |
| `agent-mux-codex-launch` | Codex `subprocess` | `e2624505-52b7-4005-b5f5-a3200b1f5a91` | PARTIAL | Launch and turn returned success; no `logs/session.log` was available to verify model output. |
| `agent-mux-opencode` | Opencode `subprocess` | `b2d2f785-3da4-4146-bb89-377df2823ce8` | PARTIAL | Launch and turn returned success; no `logs/session.log` was available to verify model output. |

All smoke sessions were stopped after testing.

## Commands Used

Structured turn-based launches:

```sh
mux launch --launch <launch-id>
mux sessions turn <session-id> "Smoke test: reply with exactly <TOKEN> and nothing else."
mux sessions stop <session-id>
```

Claude PTY:

```sh
mux launch --launch chrispian-claude-tui
mux sessions input <session-id> $'Smoke test: reply with exactly TETHER_CLAUDE_PTY_OK and nothing else.\r'
mux sessions tail <session-id> --follow=false
mux sessions stop <session-id>
```

JSON-RPC health was checked through the session health endpoint. The Codex
app-server session reported `jsonrpc_stdio: true` and a live `codex app-server`
process.
