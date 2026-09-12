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
# or install into $GOBIN for development:
make go-install
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
| `session.write` | `mux_session_create`, `mux_session_launch`, `mux_session_stop`, `mux_session_send_input`, `mux_session_send_turn`, `mux_session_resize`, `mux_logical_agent_resume` |
| `message.write` | `mux_message_send`, `mux_message_notify`, `mux_message_consume`, `mux_message_cancel`, `mux_message_mark_read`, `mux_message_archive`, `mux_message_unarchive` |
| `registry.write` | `tether_registry_register`, `tether_registry_update_self`, `tether_registry_deregister`, `tether_registry_merge`, `tether_registry_sync`, `tether_registry_binding_lease`, `tether_registry_binding_renew`, `tether_registry_binding_revoke`, `tether_registry_scoped_binding_set` |
| `groups.write` | `tether_group_create`, `tether_group_archive`, `tether_group_invite`, `tether_group_kick`, `tether_group_leave`, `tether_group_set_role`, `tether_group_post`, `tether_group_mark_read` |
| `delivery.write` | `mux_message_redrive`, `mux_message_purge` |
| `catalog.write` | `mux_agent_create`, `mux_agent_edit` |
| `ai.invoke` | `mux_ai_chat`, `mux_ai_chat_stream`, `mux_ai_embeddings` |

Scopes are per capability group, not a hierarchy — `registry.write` does not
imply `groups.write`, and neither implies `message.write`. Grant the ones the
client actually needs.

An agent participating in messaging typically needs
`message.write,registry.write` — `registry.write` to register its identity and
lease a runtime binding, `message.write` to send and consume. Add
`groups.write` only if it creates or administers group rooms; posting to a
group it already belongs to is `groups.write` as well (`tether_group_post`).

See [messaging-adoption.md](./messaging-adoption.md) for the full opt-in walkthrough.

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

## Tool annotations

Every tool publishes MCP annotation hints. A client uses them to decide what it
can run without asking, so a wrong hint is worse than a vague one.

| hint | what Tether publishes |
|---|---|
| `readOnlyHint` | **assessed per tool.** True only where the call path was traced and modifies nothing. |
| `destructiveHint` | **assessed per tool.** True only where information is irreversibly lost, not merely written. |
| `openWorldHint` | **assessed per tool.** True where the tool reaches outside the local daemon and catalog: the MCP proxy, the AI gateway. |
| `idempotentHint` | **NOT assessed. Left at its cautious default (`false`) on every tool.** |

That last row is the important one. `idempotentHint: false` here means *nobody
decided*, not *this tool is not idempotent* — several tools are idempotent and
say otherwise. Assessing repeat semantics per tool is a separate piece of work,
and shipping a thin version would be worse than not shipping one, because an
unassessed cautious default is indistinguishable from an assessed one. **Do not
read Tether's `idempotentHint` as a claim.** The other three are claims.

### How `readOnlyHint` is established

By tracing the call path to a store operation — never from the tool's name, its
scope, or the HTTP verb behind it. `mux_message_inbox` is why:

```
mux_message_inbox        ← a read verb, and unscoped
  a.client.MessageInbox  ← reads as a read
    GET /messages/inbox  ← an HTTP GET
      MessageStore.Inbox ← reads as a read
        UPDATE messages SET delivered_at   ← the truth, four layers down
```

Every signal above the last line is wrong. `mux_message_inbox` is annotated as a
write, and **retrying it is not free** — the second call consumes a different
set of messages, because the first already marked the boundary. The verb is
tracked as a defect at `CW-20260912-0114`.

`tether_group_read` is the near-miss that is genuinely a read:
`registry.ListGroupMessages` contains no write and `MarkRead` is a separate
explicit call.

### What enforces this

- A tool **cannot be registered without declaring its behavior** — it is a
  required parameter and the code will not compile without it.
- A tool claiming read-only while calling a client method classified as a write
  **fails the test suite**.
- Tools whose name reads as a read but which write are pinned by name, so none
  can be flipped by pattern-matching.

The second control reaches tools that call the daemon over its client. It does
**not** reach tools served in-process, the AI gateway, or the proxy tools — for
those the annotation rests on review. Stated because a green suite should not be
read as more coverage than it has.

### Annotations on proxied tools

Tools relayed from upstream MCP servers pass through **verbatim**, including
whatever annotations the upstream advertises. Tether does not assess or rewrite
another application's tools. Everything above describes Tether's own tools.

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
| `launch_id` | string | ✓ | Launch profile ID from the catalog (see mux_catalog_list_launches) |
| `boot_prompt` | string | — | Optional boot prompt override; replaces catalog static boot fragments verbatim |
| `agent_file` | string | — | v005-08: filesystem path to an agent YAML matching config.Agent shape. Field-merged over the catalog agent. |
| `agent_inline` | string | — | v005-08: JSON-encoded agent definition (same shape as config.Agent). Highest precedence in agent resolve order. |
| `boot_profile` | string | — | v005-08: filesystem path to a bootgen boot-profile YAML. Carries the MCP server allowlist (mcp_servers). |
| `injection` | string | — | Caller-provided JSON config.LaunchInjection (native_files + boot_dir_overlay) supplied outside catalog YAML. Caller native files append after catalog native files; caller boot-dir overlay entries win on duplicate rel_path. SECURITY: persisted at rest in launch_plans — non-secret content only; route secrets through provider env passthrough/whitelist instead. |
| `override` | string | — | v005-08: JSON object applied last over the resolved plan. Fields: system_prompt (string), env (KEY:VAL map). |
| `prompt_append` | string | — | Additional boot-prompt text appended after catalog/agent/override content. Use for narrow launch-time handoffs without replacing the base prompt. |

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
Get a message by ID. Scoped to the claimed identity — `as` must be the
message's sender or recipient, or the call returns `forbidden` (403).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID |
| `as` | string | ✓ | Caller URN asserting the read (ADR 0045) |

#### `mux_message_inbox`
Pull a recipient's undelivered messages — the atomic-delivery agent pull model.
**Destructive:** what it returns is marked delivered and will not appear in a
future inbox call. For a repeatable browse, use `mux_message_list`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `to` | string | ✓ | Recipient URN |
| `kind` | string | — | Comma-separated kind filter |
| `thread_id` | string | — | Thread ID filter |

#### `mux_message_thread`
List all messages in a thread, scoped to the ones involving the claimed
identity.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `thread_id` | string | ✓ | Thread ID |
| `as` | string | ✓ | Caller URN asserting the read (ADR 0045) |
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

### Identity and self-discovery

Added by messaging vNext. The caller identity these tools carry is
**self-asserted and unverified** (ADR 0045, same-host trust) — it is addressing
and an audit trail, not authentication.

The parameter carrying it is **not uniformly named**: `to` on
`mux_message_inbox`/`mux_message_list`, `as` on most other reads,
`authorized_by` on the repair tools below, and `by` / `member_urn` /
`from_urn` / `creator_urn` on the group tools. Each table states which.

#### `tether_whoami`
Answers "who am I, and is anything currently bound to me?" — the registered Profile, attached external-id mappings, group memberships, and the current RuntimeBinding. Every field is independently best-effort: an unregistered or never-bound identity comes back as a normal result, not an error, so this is also how a session checks whether it has an identity at all.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `as` | string | ✓ | msg:// URN to look up (self-asserted, no verification). |

---

### Registry

The federation directory — identity rows for agents and projects. It holds
identity and a callback URI, never operational content (ADR 0041 D18).

#### `tether_registry_register` _(registry.write)_
Register a new agent or project profile. **The server mints the URN** — supplying a `urn` field returns `invalid_request`. `profile` matches `registry.Profile`: `display_name` is the only required field; `title`, `role`, `description`, `avatar`, `project`, `status`, `callback`, `capabilities`, `skills`, `links`, `kind_meta`, `host_address` and `health_status` are optional. Returns the canonical Profile with the minted URN.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `kind` | string | ✓ | Entity kind: 'agent' or 'project'. |
| `profile` | object | ✓ | Profile JSON to register. See tool description for the field shape. |

#### `tether_registry_lookup`
Look up one profile by URN. Soft-deleted (`status='deprecated'`) rows **are** returned here — they are excluded only from default search results.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `urn` | string | ✓ | Full URN as minted by Register, e.g. msg://agent/agent-mux/agt_xxxxxxxxxx. |

#### `tether_registry_lookup_by`
Resolve a substrate-local external id to one registry profile. Returns 0 or 1 row. Use it when you hold a local id — a Tether catalog slug, a Cerberus owner — and need the canonical URN. Note this is the one registry tool whose `kind` also accepts `group`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `external_id` | string | ✓ | Substrate-local identifier to resolve. |
| `kind` | string | ✓ | Entity kind to resolve: 'agent', 'project', or 'group'. |
| `substrate` | string | — | Optional substrate scope such as 'tether' or 'cerberus'. |

#### `tether_registry_search`
Search by filter; all filters AND together, results ordered alphabetically on `display_name`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `kind` | string | ✓ | Entity kind to search: 'agent' or 'project'. |
| `capability` | string | — | Filter to rows that carry this capability string. |
| `project` | string | — | Filter on project (exact match). |
| `role` | string | — | Filter on role (exact match). |
| `skill_name` | string | — | Filter to rows that carry a skill with this name. |
| `status` | string | — | Filter on status. Empty → active only; 'deprecated' → deprecated only; '*' → all. |
| `title` | string | — | Filter on title (exact match). |

#### `tether_registry_update_self` _(registry.write)_
Partial-merge update. Scalar fields update column-wise — only fields present in the patch are touched. Array fields follow the patch semantics in the tool's own description; read it via `mux mcp` before relying on replace-vs-append behavior.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `patch` | object | ✓ | UpdatePatch JSON. See tool description for partial-merge semantics. |
| `urn` | string | ✓ | Full URN of the row to update. |

#### `tether_registry_deregister` _(registry.write)_
Soft-delete: status flips to `deprecated`. The row stays visible to direct lookup so callers can audit it, and drops out of default search.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `urn` | string | ✓ | Full URN of the row to soft-delete. |

#### `tether_registry_merge` _(registry.write)_
Merge a source profile into a destination: the source's external-id mappings reattach to the destination and the source row is soft-deleted. Returns the destination Profile. This is the deduplication tool — use it when two rows turn out to be the same actor.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `into` | string | ✓ | Destination URN the source's identity mappings are reattached to. |
| `urn` | string | ✓ | Source URN to merge away. |

#### `tether_registry_sync` _(registry.write)_
Refresh thin-profile columns from the row's `callback` URI. Two success shapes: `{ok:true, synced:false}` when no callback is configured, `{ok:true, synced:true, profile:<refreshed>}` when one was invoked. **Raw callback payload is never stored** — ADR 0041 D18; the registry holds identity, never operational content.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `urn` | string | ✓ | Full URN of the row to sync. |

---

### Runtime bindings

Which live session currently receives mail for an actor. An actor with no
binding still accumulates mail; it just has nowhere to be pushed right now.

#### `tether_registry_binding_lease` _(registry.write)_
Declare that a session now receives mail for `target_urn`. Always mints `visibility='published-local'`, and `capabilities` must be exactly `["pull-only"]` — caller-supplied-webhook push bridging is not implemented, so the bridge pulls its own mailbox. Refuses with `conflict` rather than superseding a binding Tether itself manages.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `attempt_id` | string | ✓ | Identifier for this specific lease attempt. |
| `capabilities` | array | ✓ | Must be exactly ["pull-only"]. |
| `host_id` | string | ✓ | Identifier for the external host/bridge process. |
| `session_id` | string | ✓ | Self-asserted session id the caller is leasing on behalf of. |
| `target_urn` | string | ✓ | msg:// target URN (session or agent). |
| `ttl_seconds` | number | — | Lease duration in seconds; 0 or omitted means no expiry. |

#### `tether_registry_binding_renew` _(registry.write)_
Extend a lease. Fails `conflict` when a newer generation already exists for the same target — which is what a crashed predecessor's replacement looks like, so treat that as legitimate takeover rather than a retryable fault.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `binding_id` | string | ✓ | Binding id returned by a prior lease. |
| `ttl_seconds` | number | — | New lease duration in seconds; 0 or omitted means no expiry. |

#### `tether_registry_binding_revoke` _(registry.write)_
Relinquish a lease. Idempotent. Good hygiene on clean shutdown; a crash simply lets the lease expire instead.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `binding_id` | string | ✓ | Binding id to revoke. |

#### `tether_registry_binding_current`
The authoritative current binding for a target — highest generation, non-revoked, non-expired.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `target_urn` | string | ✓ | msg:// target URN. |

#### `tether_registry_binding_list`
Every binding ever leased for a target, newest generation first. The audit view.

> Any same-host caller can list any target's bindings — there is no ownership check on this route today (CW-20260907-0034).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `target_urn` | string | ✓ | msg:// target URN. |

---

### Scoped role/slot bindings

Consumer-owned `(scope, slot)` → target mappings, revision-tracked.

#### `tether_registry_scoped_binding_set` _(registry.write)_
Publish a new revision of a consumer-owned `(scope, slot)` role binding — for example `scope='run-42'`, `slot='reviewer'`. **Tether does not interpret scope or slot names, and a binding grants no command authority.** It is a directory entry the consumer gives meaning to.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `created_by` | string | ✓ | Caller URN recorded as provenance for this revision. |
| `scope` | string | ✓ | Consumer-owned scope, e.g. a run or team id. |
| `slot` | string | ✓ | Role/slot name within the scope, e.g. 'reviewer'. |
| `target_urns` | array | ✓ | One or more target URNs for this slot. |

#### `tether_registry_scoped_binding_resolve`
Resolve the current revision for a `(scope, slot)`. `single=true` resolves to exactly one target and errors `conflict` on zero or several, rather than making the caller guess.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `scope` | string | ✓ | Consumer-owned scope. |
| `slot` | string | ✓ | Role/slot name within the scope. |
| `single` | boolean | — | Resolve to exactly one target (default false: return every target). |

#### `tether_registry_scoped_binding_revisions`
Every revision ever published for a `(scope, slot)`, newest first.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `scope` | string | ✓ | Consumer-owned scope. |
| `slot` | string | ✓ | Role/slot name within the scope. |

---

### Groups

Group rooms with mailbox-pull delivery, a per-member read cursor, and
server-side `@` mention parsing. See [ADR 0042](adr/0042-group-messaging.md)
and [groups/symbols.md](./groups/symbols.md).

#### `tether_group_create` _(groups.write)_
Create a group. The server mints a 3-segment URN — `msg://group/<authority>/grp_<10 alnum>` (ADR 0042; the 3-segment form was locked over the original 2-segment spec). The creator is added with `role='owner'` in the same transaction.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `creator_urn` | string | ✓ | Caller URN; must exist as an active agent/project registry row. Becomes the owner. |
| `display_name` | string | ✓ | Human-readable group name. |
| `capabilities` | array | — | Topic tags for discovery via tether_registry_search. |
| `description` | string | — | Free-form description of the group's purpose. |
| `role` | string | — | Group category (free-form), e.g. 'design-room' / 'incident-bridge' / 'project-coord'. |

#### `tether_group_lookup`
Look up a group by URN. Archived groups are still returned — callers often need their metadata. An agent URN returns `not_found`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `urn` | string | ✓ | Full group URN, e.g. msg://group/agent-mux/grp_xxxxxxxxxx. |

#### `tether_group_list_members`
Members of a group, ordered by `joined_at`, with display names hydrated from the registry.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `group_urn` | string | ✓ | Full group URN. |

#### `tether_group_list_for_member`
Groups a member belongs to, including archived ones — those stay visible to former members.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `member_urn` | string | ✓ | Full URN of the member whose group list we're fetching. |

#### `tether_group_invite` _(groups.write)_
Add a member. Owners and moderators only. The member URN must already exist in the registry.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `by` | string | ✓ | Caller URN (must be owner or moderator). |
| `group_urn` | string | ✓ | Full group URN. |
| `member_urn` | string | ✓ | Full URN of the member being invited (must exist in registry). |
| `role` | string | — | Role at invite time: 'member' (default) \| 'moderator'. |

#### `tether_group_kick` _(groups.write)_
Remove a member. Owners and moderators only. The owner cannot be removed this way — they transfer ownership with `tether_group_set_role`, then use `tether_group_leave`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `by` | string | ✓ | Caller URN (must be owner or moderator). |
| `group_urn` | string | ✓ | Full group URN. |
| `member_urn` | string | ✓ | Full URN of the member being kicked. |

#### `tether_group_leave` _(groups.write)_
Self-removal. If the leaver is the owner, another member must already hold `owner` or `moderator`, or the call is refused `forbidden` with `cannot_leave_without_owner_transfer`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `group_urn` | string | ✓ | Full group URN. |
| `member_urn` | string | ✓ | Caller URN (the leaver — must equal the caller's own identity). |

#### `tether_group_set_role` _(groups.write)_
Change a member's role. Promotion to `owner` transfers ownership and is owner-only; moderators can promote to `moderator` but no further.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `by` | string | ✓ | Caller URN (must be owner or moderator; only owner can promote to owner). |
| `group_urn` | string | ✓ | Full group URN. |
| `member_urn` | string | ✓ | Full URN of the member whose role is changing. |
| `role` | string | ✓ | New role: 'member' \| 'moderator' \| 'owner'. |

#### `tether_group_archive` _(groups.write)_
Soft-delete a group. It becomes read-only — history stays readable, new messages are refused with `locked` (423). Owner or moderator only.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `by` | string | ✓ | Caller URN (must be the group's owner or a moderator). |
| `urn` | string | ✓ | Full group URN to archive. |

#### `tether_group_post` _(groups.write)_
Post to a group. The caller must be a member and the group must not be archived. Returns `{message_id, group_seq}`.

**Symbol vocabulary** (ADR 0042, `docs/groups/symbols.md`): `@<urn>` or `@<display_name>` is a **mention** — the daemon parses the payload before commit, resolves each token via the registry, and emits a notice to the mentioned URN's personal inbox after the group message commits. An ambiguous short form returns `invalid_request` with a `candidates` list; re-issue with the full URN. `!<command>` and `:<directive>` are **reserved namespaces the daemon does not parse** — it delivers those bytes verbatim and the consuming agent decides what they mean.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `from_urn` | string | ✓ | Caller URN (must be a member of the group). |
| `group_urn` | string | ✓ | Full group URN to post into. |
| `payload` | object | ✓ | Envelope payload as a JSON object. The daemon-side mention parser scans this for '@' tokens. |
| `content_type` | string | — | MIME-ish content type for the payload (e.g. 'text/plain', 'application/json'). |
| `kind` | string | — | Envelope kind. Defaults to 'message'. Other valid values match the messaging-store kind vocabulary. |
| `thread_id` | string | — | Optional thread id — groups subdivide into threads via this field (no hierarchical URN). |

#### `tether_group_read`
Non-destructive read. **Does not bump the read cursor** — call `tether_group_mark_read` once you have acknowledged the batch.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `as` | string | ✓ | Caller URN (must be a member; identity surrogate per v060-05). |
| `group_urn` | string | ✓ | Full group URN. |
| `limit` | number | — | Max messages to return. Server default: 100. |
| `since_seq` | number | — | Lower bound on group_seq (exclusive). 0 → use caller's last_read_seq. |
| `thread_id` | string | — | Optional thread id filter. |

#### `tether_group_mark_read` _(groups.write)_
Bump the caller's read cursor. Idempotent and monotonic: a smaller `up_to_seq` is silently a no-op.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `as` | string | ✓ | Caller URN. |
| `group_urn` | string | ✓ | Full group URN. |
| `up_to_seq` | number | ✓ | New cursor value — last_read_seq becomes max(last_read_seq, up_to_seq). |

#### `tether_group_mentions`
The caller's own mention notices across every group. Usually what you want when catching up, rather than reading each room in full.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `as` | string | ✓ | Caller URN — the member whose mentions are being read. |
| `limit` | number | — | Max mentions to return. Server default: 50. |
| `since` | string | — | RFC3339 timestamp; mentions emitted after this are returned. Empty → no lower bound. |

---

### Delivery trace, repair and retention

#### `mux_message_trace`
Full delivery state for one message. Read this before theorising about a message that did not arrive — it distinguishes never-sent from sent-and-unclaimed from delivered-and-ignored, and those have nothing to do with each other.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID. |

#### `mux_message_retention_candidates`
Messages eligible for a privacy-safe body purge.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `older_than_hours` | number | — | Lookback window in hours; 0 or omitted uses the daemon's default. |

#### `mux_message_redrive` _(delivery.write)_
Re-attempt a stuck or dead-lettered delivery. A repair tool — drive it from trace evidence, not as a retry reflex.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `authorized_by` | string | ✓ | URN recorded as provenance for this repair (self-asserted, ADR 0045). |
| `message_id` | string | ✓ | Message ID, or a literal delivery id for a group-fanout recipient. |
| `new_deadline_seconds` | number | — | New delivery deadline in seconds from now; 0 or omitted means no deadline. |

#### `mux_message_purge` _(delivery.write)_
Clear one message's body and metadata, leaving its structural and trace fields (id, kind, from, to, thread, timestamps) intact. **Irreversible.** Refuses when the message still has a pending delivery obligation — including dead-lettered, which remains repairable via `mux_message_redrive` and would resend an empty message if purged first. Idempotent: purging an already-purged message reports `purged=false` rather than erroring.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `authorized_by` | string | ✓ | URN recorded as provenance for this purge (self-asserted, ADR 0045). |
| `message_id` | string | ✓ | Message ID. |

---

### Agents

Catalog agent profiles. Not messaging — listed here because `catalog.write`
appears in the scope table above.

#### `mux_agent_list`
List agent profiles in the catalog.

_No parameters._

#### `mux_agent_show`
Show one agent profile.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | ✓ | Agent ID |

#### `mux_agent_create` _(catalog.write)_
Create an agent profile in the catalog.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | ✓ | Agent ID — a single name with no path separators; becomes the YAML filename. Kebab-case recommended. |
| `agent_prompt` | string | — | Agent persona prompt (optional). |
| `name` | string | — | Human-readable name (defaults to id). |
| `project` | string | — | Catalog project ID — required when scope=project. The agent is written to that project's repo at <repo_root>/.tether/agents/. |
| `roles` | string | — | Comma-separated role list (optional). |
| `scope` | string | — | Discovery layer: project (default) \| user \| system. |
| `skills` | string | — | Comma-separated skill ID list (optional). |
| `system_prompt` | string | — | Agent system prompt (optional). |

#### `mux_agent_edit` _(catalog.write)_
Edit an existing agent profile. Only fields present in the call are changed.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | ✓ | Agent ID to edit. |
| `agent_prompt` | string | — | New persona prompt (optional). |
| `name` | string | — | New human-readable name (optional). |
| `roles` | string | — | Comma-separated role list — replaces existing roles; empty string clears them (optional). |
| `skills` | string | — | Comma-separated skill ID list — replaces existing skills; empty string clears them (optional). |
| `system_prompt` | string | — | New system prompt (optional). |

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
8. mux_ai_embeddings                 → generate embedding vectors (requires ai.invoke)
9. mux_ai_usage / mux_ai_budgets     → inspect durable usage and live budget headroom
10. mux_ai_budget_alerts             → inspect durable budget_rejection alerts directly
11. mux_ai_wait_budget_alerts        → wait briefly for live ai.budget_rejected events
12. mux_events_history               → inspect broader durable daemon/session/broker history
13. mux_events_wait                  → reuse the bounded-live pattern for live daemon/session events
14. mux_ai_audit                     → inspect broader durable audit history
```

### AI tool request forms

`mux_ai_route_preview`, `mux_ai_chat`, `mux_ai_chat_stream`, and `mux_ai_embeddings` support two request styles:

1. Shorthand text form: `text` plus optional `system_prompt`, `provider`,
   `model`, budget/correlation hints, and optional image helpers
   (`image_urls`, `image_base64`, `image_mime_type`) for chat/preview/stream.
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
  "max_output_tokens": 256,
  "image_urls": ["https://example.com/diagram.png"]
}
```

Inline image shorthand example:

```json
{
  "text": "Describe this screenshot.",
  "image_base64": "<base64 bytes here>",
  "image_mime_type": "image/png"
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

`mux_ai_chat`, `mux_ai_chat_stream`, and `mux_ai_embeddings` require the `ai.invoke` scope. `mux_ai_list_providers`,
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
  output requires the daemon's attach endpoint or `go-tether-client`. A
  polling pattern (send input → wait → read session state) works for short
  interactions.
- **In-process SQLite.** The adapter opens its own DB connection. If the daemon
  is also running, both use SQLite WAL mode — concurrent reads work fine;
  writes serialize at the DB. For heavy concurrent write workloads, run the
  daemon and use `go-tether-client` instead.
- **No MCP resources.** Only tools are exposed; MCP resources (for streaming
  file content, etc.) are not yet wired.

---

## Related

- [`docs/adr/0019-mcp-stdio-adapter.md`](adr/0019-mcp-stdio-adapter.md) — design rationale
- [`docs/api/README.md`](api/README.md) — HTTP/UDS daemon API reference
- [`docs/dev-setup.md`](dev-setup.md) — catalog schema and dev workflow
- `go-tether-client` — Go client library for external consumers (`github.com/hollis-labs/go-tether-client`)
- `examples/catalog/boot-profiles/` — example boot-profile YAMLs
