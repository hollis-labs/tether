# Tether — MCP Adapter

`mux mcp` starts an MCP stdio server that exposes the Tether runtime as
tools. Any MCP-capable client — Claude Desktop, Claude Code, Cursor, a custom
agent, a Hadron blueprint — can call session lifecycle, catalog reads,
messaging, and boot prompt generation directly from tool calls.

The adapter communicates over stdin/stdout. Your MCP client launches it as a
subprocess; no daemon needs to be running first.

---

## Quick start

### 1. Build the binary

```bash
cd ~/dev/hollis-labs/apps/tether
make build          # produces bin/mux
# or install globally:
go install ./cmd/mux
```

### 2. Add to your MCP client config

#### Claude Desktop / Claude Code (`mcp.json` or `claude_desktop_config.json`)

```json
{
  "mcpServers": {
    "tether": {
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
    "tether": {
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
prompt generation, AI provider/model inspection, route previews, and durable
AI usage/audit queries) require no authentication.

Mutating tools require a **token** and the corresponding **scope**:

| Scope | Grants access to |
|---|---|
| `session.write` | `mux_session_create`, `mux_session_launch`, `mux_session_stop`, `mux_session_send_input`, `mux_session_resize`, `mux_logical_agent_resume` |
| `message.write` | `mux_message_send`, `mux_message_consume`, `mux_message_cancel` |
| `ai.invoke` | `mux_ai_chat`, `mux_ai_chat_stream` |

Pass both via flags or environment variables:

```bash
# Flags:
mux mcp --token my-secret --scopes session.write,message.write,ai.invoke

# Environment variables:
export AGENT_MUX_MCP_TOKEN=my-secret
export AGENT_MUX_MCP_SCOPES=session.write,message.write,ai.invoke
mux mcp
```

The token value is opaque — Tether does not validate it against any external
service; it simply confirms one is present. Pick any string. If you're running
in a trusted local-only context, you can omit auth and only call read-only
tools.

---

## Proxy Mode

`mux mcp --proxy` turns Tether into an MCP gateway for upstream servers from
`<catalog>/mcp-servers/`.

| Command | Tool surface |
|---|---|
| `mux mcp` | Tether native `mux_*` tools only |
| `mux mcp --proxy` | Tether native tools, all upstream tools, `mux_discover_tools`, `mux_discover`, and `mux_call` |
| `mux mcp --proxy --servers vanta,clockwork` | Tether native tools, selected upstream tools, and discovery/call tools for hidden upstreams |
| `mux mcp --proxy --only vanta,clockwork` | Only tools from the selected upstream servers |

Use `--servers` for Tether-launched agents that may still need the control-plane
tools or the `mux_call` fallback. Use `--only` for external MCP clients where
the operator expects the named servers to be the complete native tool list.
`--only` requires `--proxy` and a non-empty server list; it suppresses native
Tether `mux_*` tools, `mux_catalog_list_mcp_servers`, `mux_catalog_refresh`,
`mux_discover_tools`, `mux_discover`, and `mux_call`.

`MUX_MCP_SERVERS` remains the environment fallback for `--servers` mode. The
explicit `--only` flag uses its own comma-separated value and does not widen
from the environment.

### Semantic Discovery

In normal proxy and `--servers` mode, `mux_discover_tools` provides a concise
tool-selection flow:

```json
{
  "intent": "create a task",
  "limit": "8"
}
```

It returns JSON grouped by upstream server/domain. Each recommendation includes
`call_name`, `server`, `summary`, up to three `tags`, `safety`, `native`, `why`,
`score`, and refs such as `tools/list:<tool>` for schemas. Use
`mux_discover` or MCP `tools/list` when you need the full input schema.

---

## Catalog setup

The adapter reads the same catalog as the rest of `mux`. Default catalog root:
`~/.tether/catalog/`. Override with `--catalog /path/to/catalog`.

Minimum catalog structure for useful MCP sessions:

```
~/.tether/catalog/
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

1. `<catalog>/skills/<id>.md`, `~/.tether/skills/<id>.md`, and project
   `.tether/skills/<id>.md`
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
are addressed using URNs in the form `msg://<kind>/<authority>/<id>[/<subid>]`.

**URN examples:**
- `msg://session/local/session-abc` — a live session mailbox
- `msg://agent/agent-mux/logical-agent-xyz` — a logical agent mailbox
- `msg://user/local/me` — the user / human inbox

**Kind values:** `request`, `response`, `notice`, `handoff`,
`status_update`, `escalation`. Use `metadata.urgency` or
`mux_message_notify.urgency` for delivery urgency: `very-low`, `low`,
`normal`, or `high`.

#### `mux_message_send` _(message.write)_

| Parameter | Type | Required | Description |
|---|---|---|---|
| `from` | string | ✓ | Sender URN |
| `to` | string | ✓ | Recipient URN |
| `kind` | string | ✓ | Message kind |
| `payload_json` | string | — | JSON payload body |
| `thread_id` | string | — | Thread ID for grouping |
| `in_reply_to` | string | — | Message ID this replies to |

#### `mux_message_notify` _(message.write)_
Send a message and best-effort wake a live recipient session with a
daemon-injected mailbox reminder turn. The message is stored even when no live
session can be resolved.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `from` | string | ✓ | Sender URN |
| `to` | string | ✓ | Recipient URN |
| `kind` | string | — | Message kind; defaults to `notice` |
| `payload_json` | string | — | JSON payload body |
| `thread_id` | string | — | Thread ID for grouping |
| `in_reply_to` | string | — | Message ID this replies to |
| `urgency` | string | — | `very-low`, `low`, `normal`, or `high`; defaults to `normal` |
| `session_id` | string | — | Explicit live session to wake |
| `wake_text` | string | — | Override the generated mailbox wake text |
| `no_wake` | boolean | — | Store only; skip wake injection |

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

These tools expose durable history stored in the agent-mux SQLite database plus
a bounded live event wait surface over the daemon SSE stream. All are read-only
and require no auth scope. They are available in both
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

#### `mux_events_history`
Query durable event-bus history across daemon, session, and broker scopes from
the shared `events` table. Returns newest first.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `scope` | string | — | Optional comma-separated scope allow-list: `daemon`, `session`, `broker` |
| `kind` | string | — | Optional comma-separated event kind allow-list |
| `session_id` | string | — | Exact session id filter |
| `since_seq` | number | — | Only return events with seq greater than this value |
| `cursor` | number | — | Pagination cursor; only return events with seq less than this value |
| `limit` | number | — | Max events (default 100, max 1000) |

```json
// Response
{
  "ok": true,
  "events": [
    {
      "seq": 12,
      "at": "...",
      "scope": "broker",
      "session_id": "sess-1",
      "kind": "broker.envelope_sent",
      "payload_json": "{\"id\":\"m1\"}"
    }
  ],
  "count": 1,
  "next_cursor": 12
}
```

#### `mux_events_wait`
Wait briefly for live daemon or session events from the running `muxd` event
stream. This is a bounded read surface for agents that need near-real-time
event reaction without keeping a long-lived stream open.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `scope` | string | — | Scope allow-list as comma-separated `daemon`, `session`, `broker`. Defaults to `daemon`. |
| `kind` | string | — | Optional comma-separated event kind allow-list. |
| `session_id` | string | — | Exact session id filter. |
| `since_seq` | number | — | Only return events with seq greater than this value. |
| `wait_ms` | number | — | Maximum wait in milliseconds (default 5000). |
| `max_events` | number | — | Maximum matching events to return (default 1, max 100). |

```json
// Response
{
  "ok": true,
  "events": [
    {
      "seq": 12,
      "kind": "session.state_changed",
      "scope": "session",
      "session_id": "sess-1",
      "payload_json": "{\"state\":\"running\"}"
    }
  ],
  "count": 1,
  "timed_out": false,
  "next_since_seq": 12
}
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

### Inspect or invoke the AI gateway

```
1. mux_ai_list_providers             → discover configured provider ids
2. mux_ai_list_models                → inspect visible models
3. mux_ai_list_routes                → inspect live planner route order
4. mux_ai_route_preview              → preview route/cost without model invocation
5. mux_ai_route_explain              → explain why each route matched or failed
6. mux_ai_chat                       → invoke the gateway and return one final response (requires ai.invoke)
7. mux_ai_chat_stream                → invoke the gateway as a live MCP stream (requires ai.invoke)
8. mux_ai_usage / mux_ai_budgets     → inspect durable usage and live budget headroom
9. mux_ai_budget_alerts              → inspect durable budget_rejection alerts directly
10. mux_ai_wait_budget_alerts        → wait briefly for live ai.budget_rejected events
11. mux_events_history               → inspect broader durable daemon/session/broker history
12. mux_events_wait                  → reuse the bounded-live pattern for live daemon/session events
13. mux_ai_audit                     → inspect broader durable audit history
```

### AI tool request forms

`mux_ai_route_preview`, `mux_ai_chat`, and `mux_ai_chat_stream` support two request styles:

1. Shorthand text form: `text` plus optional `system_prompt`, `provider`,
   `model`, and budget/correlation hints.
2. Full normalized request form: `request` (object) or `request_json`
   (JSON string). These are mutually exclusive with `text`.

The scalar hint fields still act as explicit overrides on top of a supplied
`request` object, so a caller can keep a reusable request body and pin a
different provider/model at call time.

Example shorthand call:

```json
{
  "text": "Summarize this change.",
  "system_prompt": "Be concise.",
  "provider": "anthropic-work",
  "max_output_tokens": 256
}
```

Example full request call:

```json
{
  "request": {
    "operation": "chat",
    "provider_hint": "anthropic-work",
    "input": [
      {
        "role": "system",
        "parts": [{"type": "text", "text": "Be concise."}]
      },
      {
        "role": "user",
        "parts": [{"type": "text", "text": "Summarize this change."}]
      }
    ],
    "tools": [],
    "metadata": {"surface": "mcp"}
  }
}
```

`mux_ai_chat` and `mux_ai_chat_stream` require the `ai.invoke` scope. `mux_ai_list_providers`,
`mux_ai_list_models`, `mux_ai_list_routes`, `mux_ai_route_preview`,
`mux_ai_route_explain`, `mux_ai_usage`, `mux_ai_budgets`,
`mux_ai_budget_alerts`, `mux_ai_wait_budget_alerts`, `mux_ai_audit`, and
`mux_events_history` and `mux_events_wait` are
read-only.

### AI live streaming

`mux_ai_chat_stream` bridges the daemon `/ai/chat/stream` SSE path into MCP
client notifications while the tool call is still running.

During one streaming invocation, clients can receive:

- `notifications/ai/chat_stream`
  - structured payload with `source: "mux_ai_chat_stream"` and the normalized
    `event`
- `notifications/message`
  - info-level logging notification carrying the same structured event payload
- `notifications/progress`
  - emitted when the tool call includes `_meta.progressToken`; `progress` is
    the monotonically increasing stream event count and `message` is the event
    kind

The tool result still returns the final normalized `response` plus
`event_count`, so clients that ignore live notifications still get the final
answer.

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
