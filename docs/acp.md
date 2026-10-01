# Tether — ACP Adapter

`tether acp` starts an Agent Client Protocol (ACP) server that exposes a tether
session to ACP-aware editors. Editors (Zed natively, JetBrains via `acp.json`,
Avante.nvim, CodeCompanion.nvim) spawn `tether acp` as a subprocess and drive
sessions via JSON-RPC 2.0 over newline-delimited stdio.

ACP is an open editor↔agent protocol. Spec: <https://agentclientprotocol.com>.

The adapter requires a running tether daemon (`tetherd`) — session lifecycle is
owned by the daemon, and ACP routes through it over UDS.

---

## Quick start

### 1. Install the binary

```bash
cd ~/dev/hollis-labs/apps/tether
make go-install     # installs tether to $GOBIN
```

### 2. Start the daemon

```bash
tether daemon start
```

### 3. Wire your editor

See per-editor sections below. The minimal command is:

```bash
tether acp --agent <launch_id>
```

`<launch_id>` is the name of an entry in `~/.tether/catalog/launches/`
(see `tether launch list`). The launch profile picks the agent + provider that
this ACP connection drives.

---

## Authentication

Authentication mirrors `tether mcp`: an optional bearer token + scopes. Set
either via flag or environment variable. The editor calls the ACP
`authenticate` method with `{"token":"..."}` before any session/* call.

| Flag | Env var | Default |
|---|---|---|
| `--token <bearer>` | `TETHER_ACP_TOKEN` | (empty = auth disabled, dev only) |
| `--scopes <a,b,c>` | `TETHER_ACP_SCOPES` | `session.write` |

**Available scopes:**

| Scope | Gates |
|---|---|
| `session.write` | session/new, session/prompt, session/close, session/resume |

When no token is set, all scope-gated handlers run unauthenticated. Use this
mode only on local development machines; never in shared environments.

---

## Editor integrations

### Zed (native ACP support)

Zed exposes ACP agents via `agent_servers` in its `settings.json`. The agent
name you pick here shows up in the Assistant panel's agent picker.

```json
{
  "agent_servers": {
    "Tether (Claude)": {
      "command": "tether",
      "args": ["acp", "--agent", "claude-stream"],
      "env": {
        "TETHER_ACP_TOKEN": "your-token-here"
      }
    }
  }
}
```

Open the Assistant panel (`Cmd+R`), pick "Tether (Claude)" from the agent
selector, and start chatting. The first message creates a new tether session;
subsequent messages run as turns on the same session.

For multiple launch profiles (e.g. one for Claude, one for Codex), add
multiple entries:

```json
{
  "agent_servers": {
    "Tether (Claude)":  {"command": "tether", "args": ["acp", "--agent", "claude-stream"]},
    "Tether (Codex)":   {"command": "tether", "args": ["acp", "--agent", "codex-stream"]},
    "Tether (Sandbox)": {"command": "tether", "args": ["acp", "--agent", "claude-stream-sandboxed"]}
  }
}
```

### JetBrains IDEs (via `acp.json`)

JetBrains plugins that consume ACP read an `acp.json` file at the project
root. Place this in your repo (or `~/.config/acp.json` for global default):

```json
{
  "command": "tether",
  "args": ["acp", "--agent", "claude-stream"],
  "env": {
    "TETHER_ACP_TOKEN": "your-token-here"
  }
}
```

Verify with the JetBrains "Test Agent" action (in your specific plugin's
settings) — you should see the agent's `initialize` response with
`agentInfo.name = "tether"`.

### Neovim (via Avante.nvim or CodeCompanion.nvim)

Both plugins support ACP-shaped agent commands. Add to your plugin config:

**Avante.nvim:**

```lua
require('avante').setup({
  provider = 'acp:tether',
  acp_servers = {
    tether = {
      command = 'tether',
      args = {'acp', '--agent', 'claude-stream'},
      env = {TETHER_ACP_TOKEN = 'your-token-here'},
    },
  },
})
```

**CodeCompanion.nvim:** see plugin's ACP integration docs; the command
shape is identical.

---

## What works MVP, what doesn't

### Supported

- Multi-turn chat against any tether launch profile (Claude streaming-stdio
  + Codex jsonrpc-stdio in current builds).
- Streaming responses via ACP `session/update` notifications
  (`agent_message_chunk` variant).
- Multi-client attach via `session/resume` — a second editor can join an
  existing session by ID.
- Token + scope authentication mirroring `tether mcp`.
- Clean lifecycle: `session/close` stops the session; closing the editor
  best-effort closes any owned sessions.

### Deferred (captured for post-MVP)

- **Tool-call observability.** When the underlying agent uses its own tools
  (file reads, bash commands, edits), the editor doesn't see those as ACP
  `tool_call` updates or `session/request_permission` prompts. The agent
  performs them via its own internal plumbing and the editor sees only the
  resulting message text. Captured for portfolio-wide ACP tool-call
  translation work.
- **Editor-driven filesystem / terminal.** ACP defines `fs/read_text_file`,
  `fs/write_text_file`, and `terminal/*` for the agent to drive the editor's
  workspace. Tether declines these MVP — the underlying CLI agents do their own
  fs/terminal work without round-tripping through the editor.
- **Per-session CWD respect.** ACP `session/new` carries the editor's `cwd`,
  but tether's launch profile workspace strategy applies (typically tmpdir).
  CWD is logged but doesn't override the launch profile's workspace.
- **`session/load` (history replay).** Use `session/resume` instead — same
  multi-client benefit, no replay overhead.
- **`session/list`, `session/set_mode`, `session/set_config_option`.**
  Decline → "method not found". Use `tether_session_list` over MCP for
  enumeration.
- **Image / audio / embedded resource content blocks.** Text + resource_link
  only. Other content variants in `session/prompt` return invalid_params.
- **Hard cancellation.** ACP `session/cancel` is honored at the wire level
  (handler returns `cancelled` stop reason) but the underlying agent finishes
  generating naturally — tether has no daemon interrupt primitive yet.

See ADR 0034 for the full method-mapping decision and capability lock.

---

## Multi-client attach

Two editors can drive the same tether session:

1. Editor A spawns `tether acp --agent <launch_id>` and calls `session/new` →
   gets `sessionId`.
2. Editor B spawns its own `tether acp` subprocess and calls `session/resume`
   with the same `sessionId`.
3. Both editors now stream events from the same daemon-side session.
   Either editor's `session/prompt` runs as the next turn (one turn at a
   time per session); both editors see the response stream.

The daemon-side session is independent of either ACP connection — closing
one editor doesn't terminate the session on the other.

---

## Troubleshooting

### "cannot resolve daemon address"

Start the daemon: `tether daemon up`. Verify with `tether daemon status`.

### `authenticate` returns `token mismatch`

Confirm the bearer token in the editor's env matches the token configured
on the `tether acp` flag/env. Tokens are case-sensitive and trimmed of
surrounding whitespace.

### Session `not found` after `session/resume`

The session ID must reference a session known to the running daemon. List
known sessions with `tether sessions list` (or via the MCP `tether_session_list`
tool). Sessions that have been stopped or never launched are not resumable.

### Editor sees nothing after `session/prompt`

If running with `--agent claude-stream` and the underlying `claude` CLI is
unavailable or misconfigured, no deltas will arrive on the attach stream.
Verify with `tether sessions attach <session_id>` (in another terminal) —
you should see the agent's output. Check `tether acp` stderr for warn-level
messages from the claudestream parser.

---

## See also

- ADR 0034 — ACP surface adoption (decision record + capability lock)
- ADR 0035 — `tether mcp` daemon-client routing (companion architecture)
- `docs/mcp.md` — MCP adapter (sibling consumer surface)
- `docs/agent-config-reference.md` — agent / launch profile schema (Tier-1 bundled, Tier-2 caller-provided)
- ACP spec: <https://agentclientprotocol.com>
