# Agent Mux — MCP Adapter

`mux mcp` starts an MCP stdio server that exposes the agent-mux runtime as
tools. Any MCP-capable client — Claude Desktop, Claude Code, Cursor, a custom
agent, a Hadron blueprint — can call session lifecycle, catalog reads,
messaging, and boot prompt generation directly from tool calls.

The adapter communicates over stdin/stdout. Your MCP client launches it as a
subprocess; no daemon needs to be running first.

---

## Quick start

### 1. Build the binary

```bash
cd ~/Projects-apps/agent-mux
make build          # produces bin/mux
# or install globally:
go install ./cmd/mux
```

### 2. Add to your MCP client config

#### Claude Desktop / Claude Code (`mcp.json` or `claude_desktop_config.json`)

```json
{
  "mcpServers": {
    "agent-mux": {
      "command": "/path/to/bin/mux",
      "args": ["mcp"],
      "env": {
        "AGENT_MUX_MCP_TOKEN": "your-secret-token",
        "AGENT_MUX_MCP_SCOPES": "session.write,message.write"
      }
    }
  }
}
```

#### Cursor (`~/.cursor/mcp.json`)

```json
{
  "mcpServers": {
    "agent-mux": {
      "command": "mux",
      "args": ["--catalog", "/path/to/catalog", "mcp", "--token", "your-token", "--scopes", "session.write,message.write"]
    }
  }
}
```

#### OpenCode / CLI tools

```bash
# Read-only (no auth required):
mux mcp

# With mutating tool access:
AGENT_MUX_MCP_TOKEN=your-token \
AGENT_MUX_MCP_SCOPES=session.write,message.write \
mux mcp
```

### 3. Verify

Once your client is connected, call `mux_health`:

```json
{ "ok": true, "version": "0.1.0", "projects": 3, "agents": 5, "providers": 2, "launches": 4 }
```

---

## Authentication and scopes

Read-only tools (catalog reads, session reads, message reads, health, boot
prompt generation) require no authentication.

Mutating tools require a **token** and the corresponding **scope**:

| Scope | Grants access to |
|---|---|
| `session.write` | `mux_session_create`, `mux_session_launch`, `mux_session_stop`, `mux_session_send_input`, `mux_session_resize`, `mux_logical_agent_resume` |
| `message.write` | `mux_message_send`, `mux_message_consume`, `mux_message_cancel` |

Pass both via flags or environment variables:

```bash
# Flags:
mux mcp --token my-secret --scopes session.write,message.write

# Environment variables:
export AGENT_MUX_MCP_TOKEN=my-secret
export AGENT_MUX_MCP_SCOPES=session.write,message.write
mux mcp
```

The token value is opaque — agent-mux does not validate it against any external
service; it simply confirms one is present. Pick any string. If you're running
in a trusted local-only context, you can omit auth and only call read-only
tools.

---

## Catalog setup

The adapter reads the same catalog as the rest of `mux`. Default catalog root:
`~/.agent-mux/catalog/`. Override with `--catalog /path/to/catalog`.

Minimum catalog structure for useful MCP sessions:

```
~/.agent-mux/catalog/
  global.yaml            — global settings (state_db path, socket path)
  projects/
    myproject.yaml       — project definition
  agents/
    backend.yaml         — agent profile
  providers/
    claude-stream.yaml   — provider (e.g. cli-goprovider using claude)
  launches/
    myproject-backend.yaml  — launch profile: ties project + agent + provider
  boot-profiles/         — optional: YAML files for mux_boot_generate
    myproject.backend.main.yaml
```

See `examples/catalog/` in the repo for complete working examples, and
`docs/dev-setup.md` for the full catalog schema.

---

## Tool reference

### Health

#### `mux_health`
Returns adapter version and catalog summary. No auth required.

```json
// Response
{ "ok": true, "version": "0.1.0", "projects": 3, "agents": 5, "providers": 2, "launches": 4 }
```

---

### Catalog

All catalog tools are read-only and require no auth.

#### `mux_catalog_list_projects`
List all projects defined in the catalog.

#### `mux_catalog_list_agents`
List all agent profiles.

#### `mux_catalog_list_providers`
List all provider definitions.

#### `mux_catalog_list_launches`
List all launch profiles. Each launch combines a project, agent, and provider.

```json
// Response
{
  "ok": true,
  "launches": [
    { "id": "myproject-backend", "project": "myproject", "agent": "backend", "provider": "claude-stream" }
  ]
}
```

#### `mux_catalog_list_boot_profiles`
List boot-profile YAMLs from `<catalog>/boot-profiles/`. Used with `mux_boot_generate`.

---

### Sessions

#### `mux_session_list`
List sessions with optional filtering.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `state` | string | — | Filter: `created`, `running`, `stopped`, `failed` |
| `cursor` | string | — | RFC3339 pagination cursor |
| `limit` | number | — | Max results (default 50, max 200) |

#### `mux_session_get`
Get a single session by ID.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

#### `mux_session_create` _(session.write)_
Create a session from a launch profile. Session starts in `created` state; it
is not running yet. Follow with `mux_session_launch`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `launch_id` | string | ✓ | Launch profile ID (see `mux_catalog_list_launches`) |
| `boot_prompt` | string | — | Override the catalog's static boot fragments |

```json
// Response
{ "ok": true, "session_id": "01abc...", "workspace": "/home/user/.agent-mux/workspaces/...", "log": "..." }
```

#### `mux_session_launch` _(session.write)_
Start a previously created session. Transitions `created → running`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID from `mux_session_create` |

#### `mux_session_stop` _(session.write)_
Send a stop signal to a running session.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

#### `mux_session_wait`
Block until the session exits. Returns the exit code. Read-only; no scope required.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

```json
// Response
{ "ok": true, "session_id": "01abc...", "exit_code": 0 }
```

#### `mux_session_send_input` _(session.write)_
Send raw text to a running session's stdin (PTY). A newline is **not**
appended automatically — include `\n` if you want to submit a command.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |
| `input` | string | ✓ | Text to send |

#### `mux_session_resize` _(session.write)_
Resize the PTY terminal for a running session.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |
| `rows` | number | ✓ | Terminal rows (1–65535) |
| `cols` | number | ✓ | Terminal columns (1–65535) |

---

### Logical agents

A logical agent is a durable identity that persists across sessions and
accumulates checkpoints. Resuming a logical agent starts a new session with
its most recent checkpoint injected as boot context.

#### `mux_logical_agent_list`
List all registered logical agents.

#### `mux_logical_agent_resume` _(session.write)_
Resume a logical agent from its most recent checkpoint.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `logical_agent_id` | string | ✓ | Logical agent ID |

```json
// Response
{ "ok": true, "session_id": "01xyz...", "workspace": "...", "log": "...", "provider_id": "claude-stream" }
```

---

### Messaging

The messaging tools wrap the `go-messaging` store (migration 0010). Messages
are addressed using URNs in the form `urn:<namespace>:<id>`.

**URN examples:**
- `urn:agent:nanite:session-abc` — a Nanite session
- `urn:agent:mux:logical-agent-xyz` — an agent-mux logical agent
- `urn:user:me` — the user / human inbox

**Kind values:** `request`, `reply`, `notification`, `handoff`, `status_update`, `escalation`

#### `mux_message_send` _(message.write)_

| Parameter | Type | Required | Description |
|---|---|---|---|
| `from` | string | ✓ | Sender URN |
| `to` | string | ✓ | Recipient URN |
| `kind` | string | ✓ | Message kind |
| `payload_json` | string | — | JSON payload body |
| `thread_id` | string | — | Thread ID for grouping |
| `in_reply_to` | string | — | Message ID this replies to |

#### `mux_message_get`
Get a message by ID.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID |

#### `mux_message_inbox`
List messages in a recipient's inbox.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `to` | string | ✓ | Recipient URN |
| `kind` | string | — | Comma-separated kind filter |
| `thread_id` | string | — | Thread ID filter |

#### `mux_message_thread`
List all messages in a thread.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `thread_id` | string | ✓ | Thread ID |
| `kind` | string | — | Comma-separated kind filter |

#### `mux_message_consume` _(message.write)_
Mark a message consumed by the recipient.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID |
| `as` | string | ✓ | Recipient URN consuming the message |

#### `mux_message_cancel` _(message.write)_
Cancel a pending message.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID |

---

### Boot prompt generation

#### `mux_boot_generate`
Generate a boot prompt for an agent profile and return it as a string. The
profile YAML in `<catalog>/boot-profiles/<id>.yaml` defines how to assemble
slot content from static files, shell commands, and HTTP endpoints.

Read-only; no auth required.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `profile_id` | string | ✓ | Boot profile ID (see `mux_catalog_list_boot_profiles`) |

```json
// Response
{
  "ok": true,
  "profile_id": "nanite.backend.main",
  "boot_prompt": "# Session Boot — Nanite Backend\n\n..."
}
```

**Common workflow:**

```
mux_catalog_list_boot_profiles   → discover available profiles
mux_boot_generate (profile_id)   → get assembled boot prompt
mux_session_create (launch_id, boot_prompt=<above>)   → create session with it
mux_session_launch (session_id)  → start
```

---

## Common workflows

### Launch a new agent session

```
1. mux_catalog_list_launches          → pick a launch_id
2. mux_session_create (launch_id)     → get session_id
3. mux_session_launch (session_id)    → start the agent
4. mux_session_wait  (session_id)     → block until done
```

### Launch with a generated boot prompt

```
1. mux_catalog_list_boot_profiles     → pick a profile_id
2. mux_boot_generate (profile_id)     → get boot_prompt text
3. mux_session_create (launch_id, boot_prompt)
4. mux_session_launch (session_id)
```

### Resume a logical agent from checkpoint

```
1. mux_logical_agent_list             → find the logical_agent_id
2. mux_logical_agent_resume (id)      → starts session with checkpoint context injected
```

### Send a cross-agent message

```
1. mux_message_send (from, to, kind, payload_json)   → get message_id
2. mux_message_inbox (to)            → recipient polls inbox
3. mux_message_consume (message_id, as)              → mark consumed
```

---

## Limitations (v0.1.0)

- **No PTY output streaming.** `mux_session_send_input` sends input; reading
  output requires the daemon's attach endpoint or `go-agentmux-client`. A
  polling pattern (send input → wait → read session state) works for short
  interactions.
- **In-process SQLite.** The adapter opens its own DB connection. If the daemon
  is also running, both use SQLite WAL mode — concurrent reads work fine;
  writes serialize at the DB. For heavy concurrent write workloads, run the
  daemon and use `go-agentmux-client` instead.
- **No MCP resources.** Only tools are exposed; MCP resources (for streaming
  file content, etc.) are not yet wired.

---

## Related

- [`docs/adr/0019-mcp-stdio-adapter.md`](adr/0019-mcp-stdio-adapter.md) — design rationale
- [`docs/api/README.md`](api/README.md) — HTTP/UDS daemon API reference
- [`docs/dev-setup.md`](dev-setup.md) — catalog schema and dev workflow
- `go-agentmux-client` — Go client library for external consumers (`github.com/hollis-labs/go-agentmux-client`)
- `examples/catalog/boot-profiles/` — example boot-profile YAMLs
