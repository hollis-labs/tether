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

### Skills

#### `mux_skill_get`
Load a skill by id and return its instructions. This is the provider-neutral
counterpart to Claude Code's built-in skill loader: when a boot prompt lists a
pointer such as `/refactor-go — Apply Go refactoring patterns`, non-Claude
providers can call `mux_skill_get` with `skill_id: "refactor-go"` and follow the
returned body.

Read-only; no auth required.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `skill_id` | string | ✓ | Skill id from the boot prompt, with or without the leading `/` |

Resolution order follows Tether's layered skill discovery and then legacy
skill locations:

1. `<catalog>/skills/<id>.md`, `~/.agent-mux/skills/<id>.md`, and project
   `.agent-mux/skills/<id>.md`
2. `~/.tether/skills/<id>.md`
3. `~/.nanite/skills/<id>.md`

```json
// Response
{
  "ok": true,
  "id": "refactor-go",
  "name": "Refactor Go",
  "description": "Apply Go refactoring patterns.",
  "triggers": ["refactor", "cleanup"],
  "body": "Prefer small, tested changes.",
  "path": "/home/user/.nanite/skills/refactor-go.md",
  "layer": "legacy-nanite"
}
```

#### `mux_skill_list`
List every skill visible through the same resolver used by `mux_skill_get`.
Returns metadata only; call `mux_skill_get` to load the full body.

Read-only; no auth required.

```json
// Response
{
  "ok": true,
  "items": [
    {
      "id": "refactor-go",
      "name": "Refactor Go",
      "description": "Apply Go refactoring patterns.",
      "triggers": ["refactor", "cleanup"],
      "path": "/home/user/.nanite/skills/refactor-go.md",
      "layer": "legacy-nanite"
    }
  ],
  "meta": { "returned": 1 }
}
```

#### `mux_skill_broker`
Return ranked skill recommendations for a specific task, role, project, or
trigger set. This is the progressive-discovery companion to `mux_skill_list`:
it returns metadata, ranking, and reasons, then the caller uses
`mux_skill_get` only for the chosen skill body.

Read-only; no auth required.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `query` | string |  | Free-text task or intent |
| `role` | string |  | Optional requester role signal |
| `project` | string |  | Optional project signal |
| `task_id` | string |  | Optional Torque task id for forward-compatible enrichment |
| `triggers` | string |  | Optional comma-separated preferred trigger terms |
| `layers` | string |  | Optional comma-separated layer filter |
| `limit` | integer |  | Optional max results, default 5, max 20 |

```json
// Response
{
  "ok": true,
  "items": [
    {
      "id": "refactor-go",
      "name": "Refactor Go",
      "description": "Apply Go refactoring patterns.",
      "triggers": ["refactor", "cleanup"],
      "path": "/home/user/.nanite/skills/refactor-go.md",
      "layer": "legacy-nanite",
      "score": {
        "query_matches": 1,
        "signal_matches": 1,
        "preferred_matches": 2,
        "priority": 10
      },
      "reasons": [
        "matched query: refactor",
        "matched role/project: backend",
        "preferred triggers: refactor"
      ],
      "next": "mux_skill_get"
    }
  ],
  "meta": {
    "returned": 1,
    "total_visible": 8,
    "filters": {
      "query": "refactor handler",
      "role": "backend",
      "project": "",
      "task_id": "",
      "triggers": ["refactor"],
      "layers": []
    },
    "progressive_discovery": true,
    "task_context_resolved": false
  }
}
```

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
slot content from static files, role summaries, skill indexes, shell commands,
and HTTP endpoints. Use `type: skill_index` for the `skills` slot to emit compact
`/skill-id — description` pointers instead of inlining full skill bodies.
Use `type: role_summary` for the `agent` slot to emit role identity, a mission
paragraph, and a pointer to the full role file.

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

### Observation (durable history)

These tools expose durable history stored in the agent-mux SQLite database.
All four are read-only and require no auth scope. They are available in both
normal and `--proxy` mode.

> **Note:** `mux_events_tool_calls` (proxy mode only) is now backed by the
> same durable `proxy_events` SQLite table as `mux_proxy_events`. Results
> survive daemon restarts.

#### `mux_session_events`
List historical lifecycle events for a session in descending seq order.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |
| `limit` | number | — | Max events (default 100, max 1000) |
| `cursor` | number | — | Smallest seq from previous page (for pagination) |

```json
// Response
{
  "ok": true,
  "events": [{"seq": 12, "at": "...", "scope": "session", "kind": "session.state_changed", ...}],
  "count": 1,
  "next_cursor": 0
}
```

#### `mux_session_checkpoints`
List checkpoints for a session (resolved via its logical agent). Newest first.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

```json
// Response
{ "ok": true, "checkpoints": [...], "count": 2 }
```

#### `mux_session_attachments`
List client attach/detach records for a session. `detached_at` is `""` for
still-open or pre-tracking attachments.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

```json
// Response
{ "ok": true, "attachments": [{"id":"...","session_id":"...","client_kind":"cli","attached_at":"...","detached_at":"..."}], "count": 1 }
```

#### `mux_proxy_events`
Query durable proxy/tool call events from the SQLite store.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | — | Filter by session ID |
| `server` | string | — | Filter by upstream server ID (exact match) |
| `tool_name` | string | — | Filter by tool name prefix |
| `errors_only` | bool | — | When true, only return failed calls |
| `limit` | number | — | Max results (default 100, max 500) |
| `since` | string | — | RFC3339 lower-bound on event timestamp |

```json
// Response
{ "ok": true, "events": [...], "count": 5 }
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
