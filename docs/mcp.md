# Tether — MCP Adapter

For short guides on connecting, discovery, grants, protection and budgets,
start with [Use MCP through Tether](agents/mcp/README.md). This page is the full
adapter and tool reference.

`tether mcp` starts an MCP stdio server that exposes the Tether runtime as
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
make build          # produces bin/tether
# or install into $GOBIN for development:
make go-install
```

### 2. Add to your MCP client config

#### Claude Desktop / Claude Code (`mcp.json` or `claude_desktop_config.json`)

```json
{
  "mcpServers": {
    "tether": {
      "command": "/path/to/bin/tether",
      "args": ["mcp"],
      "env": {
        "TETHER_MCP_TOKEN": "your-secret-token",
        "TETHER_MCP_SCOPES": "session.write,message.write"
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
      "command": "tether",
      "args": ["--catalog", "/path/to/catalog", "mcp", "--token", "your-token", "--scopes", "session.write,message.write"]
    }
  }
}
```

#### OpenCode / CLI tools

```bash
# Read-only (no auth required):
tether mcp

# With mutating tool access:
TETHER_MCP_TOKEN=your-token \
TETHER_MCP_SCOPES=session.write,message.write \
tether mcp
```

### 3. Verify

Once your client is connected, call `tether_health`:

```json
{ "ok": true, "version": "0.1.0", "projects": 3, "agents": 5, "providers": 2, "launches": 4 }
```

---

## Running proxy and leaf launch observations

MCP `serverInfo.version`, the proxy's upstream `clientInfo.version`, and
`tether_health.version` use metadata embedded in the running `tether` program. Builds
made with the Makefile include its version, commit and build date; ordinary
`go build` / `go install` can report `dev` or the module version. Replacing a
file on disk does not change an existing process's reported build.

`tether_health.runtime` and downstream initialize's
`capabilities.experimental["hollis-labs.dev/mcp-runtime"]` identify that process:
schema version 1, a per-process UUID (`instance_id`), PID, observation time and
build metadata. The UUID distinguishes instances even if a PID is reused. It
is not an authentication credential. `build.image_identity` is **unknown**:
version labels, commits (including dirty builds), and build dates are not a
verified digest of the running image. This slice never reads the mutable
executable pathname to claim running-image identity.

For each actual stdio child, the proxy sends an observation in upstream
initialize's same experimental capability key:

```json
{
  "schema_version": 1,
  "mode": "observation-only",
  "owner": { "schema_version": 1, "instance_id": "...", "pid": 123,
    "observed_at": "...", "build": { "version": "dev", "go_version": "...",
      "image_identity": "unknown", "image_identity_reason": "running-image-digest-unavailable" } },
  "launch": { "selector": "/opt/bin/tesseract", "selector_kind": "absolute",
    "resolved_path": "/opt/releases/tesseract-A", "resolution": "pre-spawn-path-observation",
    "relaunch_lookup": "selector", "target_relation": "unknown", "pid": 124, "observed_at": "..." },
  "recovery": { "mechanism": "stdio-exit-bounded-retry", "attempts_used": 0,
    "attempt_limit": 5, "attempts_remaining": 5, "exit_permitted": false, "reservation": "none" }
}
```

The selector is retained separately from its symlink-resolved path. Resolution
is observed before spawn and may race with replacement; it does not identify
the image that actually ran. `target_relation` remains unknown because a
script or native wrapper can launch another program. A consumer must establish
the relationship to its own running image before comparing replacements.
Missing resolution is unknown; credential-bearing paths are omitted with
`resolution: "redacted"`. Arguments and environment values are never included.

For a PATH command, `selector_kind` is `path` and `relaunch_lookup` is
`proxy-PATH`. For a relative path they are `relative` and
`proxy-working-directory`. `exec.Command` uses the **proxy's** PATH before child
environment overrides are applied. These selectors are resolved again by the
owner on retry; the child's PATH or current directory cannot establish what
the owner will launch next. This v1 observation does not publish that private
environment or provide an owner-resolution query. A consumer that cannot
resolve the actual next candidate must report unknown.

`tether_health.upstream_servers` and `tether_gateway_status` expose
`last_launch` and a current `recovery` snapshot. Initialize's snapshot is only
for that handshake; it does not update when the retry budget resets or is
consumed. **No snapshot reserves a replacement attempt or permits exit.**
`exit_permitted` is always false and `reservation` is always `none`. Remote
HTTP/SSE connections receive no local relaunch claim. Curated `--only` keeps
its existing tool surface while still reporting the owner in initialization.

CW-20260913-0013 must provide verified running-image/candidate comparison at the
shared consumer boundary. CW-20260912-0028 remains separately gated on current
owner admission and product-specific safe draining. This observation contract
does not enable self-exit, change recovery delays, or authorize a live rollout.

## Authentication and scopes

Read-only tools (catalog reads, session reads, message reads, health, boot
prompt generation, AI provider/model inspection and route previews) require no
authentication. The AI usage, budget and audit reads (`tether_ai_usage`,
`tether_ai_budgets`, `tether_ai_audit`, `tether_ai_budget_alerts`,
`tether_ai_wait_budget_alerts`) require a token, as mutating tools do, but no
scope: any configured token is enough.

Mutating tools require a **token** and the corresponding **scope**:

| Scope | Grants access to |
|---|---|
| `session.write` | `tether_session_create`, `tether_session_launch`, `tether_session_stop`, `tether_session_send_input`, `tether_session_send_turn`, `tether_session_resize`, `tether_logical_agent_resume` |
| `message.write` | `tether_message_send`, `tether_message_notify`, `tether_message_consume`, `tether_message_cancel`, `tether_message_mark_read`, `tether_message_archive`, `tether_message_unarchive` |
| `registry.write` | `tether_registry_register`, `tether_registry_update_self`, `tether_registry_deregister`, `tether_registry_merge`, `tether_registry_sync`, `tether_registry_binding_lease`, `tether_registry_binding_renew`, `tether_registry_binding_revoke`, `tether_registry_scoped_binding_set` |
| `groups.write` | `tether_group_create`, `tether_group_archive`, `tether_group_invite`, `tether_group_kick`, `tether_group_leave`, `tether_group_set_role`, `tether_group_post`, `tether_group_mark_read` |
| `delivery.write` | `tether_message_redrive`, `tether_message_purge` |
| `catalog.write` | `tether_agent_create`, `tether_agent_edit` |
| `ai.invoke` | `tether_ai_chat`, `tether_ai_chat_stream`, `tether_ai_embeddings` |

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
tether mcp --token my-secret --scopes session.write,message.write,ai.invoke

# Environment variables:
export TETHER_MCP_TOKEN=my-secret
export TETHER_MCP_SCOPES=session.write,message.write,ai.invoke
tether mcp
```

The token value is opaque — Tether does not validate it against any external
service; it simply confirms one is present. Pick any string. If you're running
in a trusted local-only context, you can omit auth and only call read-only
tools.

---

## Proxy Mode

`tether mcp --proxy` turns Tether into an MCP gateway for upstream servers from
`<catalog>/mcp-servers/`.

| Command | Tool surface |
|---|---|
| `tether mcp` | Native Tether targets plus gateway status; flat by default |
| `tether mcp --proxy` | Native targets and every enabled upstream tool, plus status |
| `tether mcp --proxy --servers torque,tesseract` | Native targets and only those upstreams, plus status |
| `tether mcp --proxy --discovery-mode search` | `tether_tool_search`, `tether_tool_list`, `tether_tool_call`, `tether_gateway_status` |
| `tether mcp --proxy --only torque,tesseract` | Only those upstream targets plus gateway infrastructure for the selected mode |

`--servers` is an upstream restriction in **both** modes. Excluded servers are
not started, their credentials are not resolved, and no discovery, hydration,
dispatch or refresh path can reach them. Unknown or disabled IDs are hard errors.
Omission selects every enabled upstream; an explicitly empty list selects none.
`TETHER_MCP_SERVERS` is the environment fallback; `--only` uses its own nonempty
list and omits native Tether targets. `--confine` also makes an omitted list select
none (the generated agent configs retain this explicit grant boundary).

There is no hybrid surface or hidden fallback. `--broker` is removed and fails
as an unknown flag. Flat mode preserves each real tool's client-visible identity
for permissions and hooks. Search mode reduces the initial schema surface, but
client rules/hooks see **`tether_tool_call`**, rather than its downstream name.
The dispatcher may mutate or irreversibly remove state, so it advertises destructive behavior rather than read-only safety. Its inner arguments receive the same sanitization as direct calls; tool-call events record the target name and origin.

### Selecting discovery mode

Precedence (highest first): `--discovery-mode` → `TETHER_MCP_DISCOVERY_MODE` →
profile hook → `global.yaml` `mcp.discovery_mode` → persisted Tether setting →
`flat`. Only `flat` and `search` are valid; explicit empty/unknown values are
errors, **even in an overridden tier**. Same-tier explicit selectors must agree;
an argument can legitimately override a different environment value.

```yaml
# catalog/global.yaml
mcp:
  discovery_mode: flat
```

The global persisted fallback is read/written at `GET` / `PUT /settings/mcp`:
`{"discovery_mode":"search"}` sets it and `{}` clears it. It lives in the existing
settings store; daemon-only proxies read it over the daemon API, never by opening
the state DB. Mode/source resolve once at startup; edits affect newly started
endpoints. An older daemon returning 404 for this setting means no persisted fallback; other errors fail startup. Invalid gateway mode values fail gateway startup and appear in `tether doctor`, while generic catalog loading and daemon startup remain available. `tether_gateway_status` reports the effective mode and its source.

The profile tier is a typed `mcp.profiles.<id>.discovery_mode` hook in this change;
profile selection/filtering is CW-20260926-0008. No profile is selected yet.
HTTP MCP endpoint/query/header selectors belong to CW-20261001-0539. No client
brand or inventory-size heuristic changes the selected mode.

### Agents Tether launches: strict config and an allow-list

Two defaults limit which MCP servers a launched agent sees.

**Claude loads only the planted proxy.** Claude Code merges servers from
`--mcp-config`, the operator's `~/.claude.json`, project `.mcp.json` files and
the claude.ai connectors on the logged-in account. Tether plants one config per
agent (its own `tether` proxy), and every Claude launch adds
`--strict-mcp-config`, so nothing else loads. This covers the first turn and
every later turn and resume. It does not cover `tether boot`, which runs your own
Claude in your own terminal.

**This also drops the claude.ai account connectors** (Claude Docs, Google
Calendar, Drive, Gmail and the like) from a Tether-launched agent. That is
intended: an agent gets what Tether plants. Checked with a real login:
`claude -p … --strict-mcp-config` with no `--mcp-config` reports
`mcp_servers: []`. If an agent needs a connector, it has to be given to it as
an upstream in the catalog; turning strict mode off to get one also brings back
everything in `~/.claude.json`.

To turn it off, set `TETHER_CLAUDE_STRICT_MCP=0` (or `false`) in **tetherd's**
environment and restart the daemon. It is on by default. When it is off:

- tetherd logs `WARN` at startup;
- `GET /health` reports `hardening.claude_strict_mcp: false` with a reason;
- `tether doctor` shows `claude-strict-mcp` as a warning. It asks the running
  daemon, so it reflects what the daemon runs with, not doctor's own shell.

An older daemon that has no `hardening` field makes doctor warn too, because it
cannot say strict MCP is on.

This is an interim flag. It goes when go-agent-wrapper has an option that does
the same.

**The proxy reaches only a granted list of upstreams.** A launched agent's
`tether mcp --proxy` runs with `--confine`. `--servers` /
`TETHER_MCP_SERVERS` is always an allow-list: upstreams outside it are not started, their
secrets are not resolved into the agent's proxy, and `tether_tool_call` and
`tether_tool_search` cannot reach them. This holds for the operator's proxy too;
there is no hidden inventory behind a filtered flat listing.

The default list is `torque` and `tesseract`. Grant others per project
(`mcp.servers` in the project YAML) or per launch (`mcp_servers` in a boot
profile). A list you set **replaces** the default, so keep `torque` and
`tesseract` in it. A project that already lists upstreams keeps exactly that
list when you upgrade; the default applies only where no list is set. Check that
a list you wrote earlier still names `torque` and `tesseract` if its agents
use them. `cerberus` can reach hosts and containers, so it is never in
the default; an agent that needs it must be given it by name.

tether's own native tools (`tether_*`) are not upstreams and are not affected.

A resumed session (`POST /logical-agents/{id}/resume`) gets the project's
`mcp.servers` list, or the default, not the list the original launch had. A
boot profile's `mcp_servers` is applied when a session is created, and resume
re-resolves the launch from the catalog without it. So a list granted to one
launch alone is gone on resume (the agent has less), and a list narrowed below the
project's for one launch is the project's list on resume (the agent has what the
project grants, never more).

The allow-list limits what an agent's own proxy offers. It is not a boundary
against a hostile agent: see [SECURITY.md](../SECURITY.md#agents-run-as-your-user).

### Daemon-only mode (`--daemon-only`)

`tether mcp --daemon-only` never opens Tether's state database. Tether plants it
in every agent it launches, where the server runs inside the agent's sandbox:
the agent can then be kept from writing the state directory. In this mode:

- Every read and write of Tether state goes to the running daemon over its API
  (`tetherd.sock`). The server loads the catalog from disk and nothing else.
- Each tool call is recorded by the daemon. The server posts a start and an end
  record to `POST /proxy/events` with `publish`, and the daemon writes
  `proxy_events` and the `events` log itself.
- It refuses to start unless the daemon answers, with the error `tether daemon
  unreachable; tether tools unavailable`, and exits with that line alone (no usage
  text). An agent launched while `tetherd` is down therefore has **no tether tools and
  no proxied upstream tools** (`torque`, `tesseract` and the rest): the whole
  server is absent, not only its native tools. It does not retry, and nothing
  falls back to opening the database.
- If the daemon goes down after the server has started, each read fails with a
  `daemon_unavailable` tool error; tool-call records are dropped with a logged
  warning; and an upstream call goes out without its `tether.provenance`
  stamp, because the session's workstream cannot be looked up (a WARN is
  logged, and the call is not failed).
- Reads of the daemon's own state are the daemon's answers: `tether_session_health`
  now reports the daemon's live sessions, and `tether_session_list` and
  `tether_session_get` carry the daemon's `attached_clients`. `tether_logical_agent_list`
  returns the daemon's summary, which has fewer fields than the table row an
  ordinary `tether mcp` returns, and **different JSON keys**: `id`, `name`,
  `launch_id`, `checkpoint_policy` (normalized) and `checkpoint_status`, where
  the ordinary server returns `ID`, `Role`, `Name`, `Responsibilities`, … in
  Go field case. A caller that reads those keys must handle both.

An operator's own `tether mcp` (in `~/.claude.json`, say) is unchanged: it opens
the database as before. `tether boot-exec` plants the same ordinary server, since
it runs in the operator's terminal outside any Tether sandbox.

Records the daemon-only server posts are asserted by the agent's process. The
daemon checks their shape, caps their size, stamps their time, and refuses a
published record whose `session_id` names no session. It cannot tell which
session is really calling, so the tool name, server, outcome and session are the
caller's word until `tetherd` verifies who is calling (CW-20260930-0253). The
server cuts a tool error to 4 KiB, with a `…[truncated]` marker, before
sending, because the daemon refuses an oversized body whole.

### Upstream failures and recovery

Each stdio upstream has its own supervisor. After a confirmed process exit,
the proxy reconnects that server, initializes it, and replaces its tool list
and client connection. Other upstreams retain their connections. Clean exit,
nonzero exit, and signal termination all receive the same bounded retry
policy; a clean exit is not interpreted as a request to replace a stale binary.

There are at most five restart attempts, delayed by 1, 2, 4, 8, and 16 seconds.
A successful handshake does not reset the budget: the connection must remain
open for 60 seconds. Startup failures also consume this budget. After exhaustion,
correct the upstream failure and start a new proxy session. `tether_catalog_refresh`
refreshes tool schemas on existing connections; it does not reset the budget.
Initialization and tool-list requests have a 10-second timeout.

The supervisor sends no process signals. If stdout closes while the child is
still alive, the connection closes and recovery waits for actual process exit.
Likewise, a child that ignores stdin EOF is not forcibly terminated. Closing the
proxy cancels pending restart timers. This policy supervises local stdio children;
it does not add remote HTTP/SSE reconnection or periodic liveness probes.

In normal proxy modes, `tether_health` returns `ok: false` and names
`unavailable_servers` when an observed connection has failed or is recovering.
`tether_gateway_status` includes each selected origin's connection status,
catalogued/available counts and any connection error. Native `tether_health`
also retains detailed restart/exit diagnostics (call it through the dispatcher
in search mode). Cached definitions do not establish availability. These are observed states, not active probes.

`tether_tool_search` and `tether_tool_list` exclude unavailable upstreams from search
results and report `complete: false` with the missing servers. An empty result
with incomplete discovery does not establish that a product has no matching tool.
Cached native tool registrations fail explicitly while recovery is pending.

The proxy continuously drains stderr and retains an 8 KiB tail for status reads,
redacting values supplied by the catalog's environment and token configuration,
plus argument values resolved from secret references or `${VAR}` substitutions.
This includes incomplete value prefixes at the end of a live snapshot and
fragments at the start of a truncated tail.
This is bounded process-local diagnostic retention, not durable logging or a
general secret detector. Connection loss, restart scheduling, and retry exhaustion
also produce diagnostics on the proxy's stderr. In curated `--only` mode, gateway status still reports availability.

An in-flight call fails when its transport closes and is **never replayed**.
Its error says execution outcome may be unknown: a side effect can complete
before the response is lost. Calls rejected during recovery say they were not
sent. After reconnection, subsequent calls use the new client, and changed tool
schemas follow the existing tool-list notification path.

### Semantic Discovery

In search mode, query the eligible inventory:

```json
{"query":"create a task","detail":"summary","limit":8}
```

`tether_tool_search` accepts a nonempty `query`, optional `servers` and AND `tags`
arrays, `detail` (`summary` or `schema`), integer `limit` (1..50; default 10), and
`cursor`. It keeps the current keyword scorer behind a service seam until the
shared ranker has a tagged release. Filters narrow the endpoint's inventory.

`tether_tool_list` enumerates real schemas with optional `servers`, integer
`limit` (1..100; default 50) and `cursor`. Alternatively, use `{"names":["torque_task_get"]}`
for exact-name hydration; names and enumeration fields are mutually exclusive.
Unknown, excluded or unavailable names yield per-name errors without schemas.

Both return `items`, `returned`, `total_matches`, `truncated`, `next_cursor`,
`complete` and `unavailable_servers`. Items contain the exact `name`, `origin`,
`title`, `description` and verbatim `annotations`; schema detail includes real
`inputSchema`, `outputSchema` and `_meta`. Search adds `score`. Native targets
have origin `tether` and participate in both enumeration and search. Enumeration
is sorted by final name; search sorts by score then name. Cursors bind mode/source,
filters and the inventory/availability snapshot. A stale cursor fails with restart
guidance. No guessed safety labels or inaccessible schema/example references are
returned. Tool TTL/cache fields unsupported by the negotiated SDK are not invented.

`tether_tool_call` takes `{"name":"torque_task_get","arguments":{"id":"CW-..."}}`.
Only an eligible exact name dispatches. Upstream content, tool-error content and
structured output are preserved. `tether_gateway_status` accepts optional `name`
to explain why it is not directly visible (mode, restriction or availability).
Profile exclusion details, collision/lint findings and ordering pins land in the
subsequent profile/metadata tasks.

---

## Catalog setup

The adapter reads the same catalog as the rest of `tether`. Default catalog root:
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
  boot-profiles/         — optional: YAML files for tether_boot_generate
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
scope, or the HTTP verb behind it. `tether_message_inbox` is why:

```
tether_message_inbox        ← a read verb, and unscoped
  a.client.MessageInbox  ← reads as a read
    GET /messages/inbox  ← an HTTP GET
      MessageStore.Inbox ← reads as a read
        UPDATE messages SET delivered_at   ← the truth, four layers down
```

Every signal above the last line is wrong. `tether_message_inbox` is annotated as a
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

#### `tether_health`
Returns adapter version and catalog summary. No auth required.

```json
// Response
{
  "ok": true,
  "version": "0.1.0",
  "projects": 3,
  "agents": 5,
  "providers": 2,
  "launches": 4,
  "catalog_read": {
    "status": "current",
    "source": "catalog_root",
    "observed_at": "2026-09-13T12:00:00Z",
    "validated": true
  }
}
```

---

### Catalog

All catalog tools are read-only and require no auth.

`tether_health`, `tether_catalog_list_projects`, `tether_catalog_list_agents`,
`tether_catalog_list_providers`, and `tether_catalog_list_launches` reload and validate
the layered launch catalog for every call. A running MCP adapter therefore sees
valid file edits on the next read without a process restart. Each successful
response includes `catalog_read` with its source, observation time, and
validation status.

Each call uses one fully loaded catalog value, and that generation passes
project, agent, provider, and launch cross-reference validation as a whole.
Catalog files do not form a filesystem transaction; write individual files
atomically and keep intermediate states valid when coordinating several files.
If a call observes a malformed or incomplete edit, catalog list tools return an
MCP error with code
`catalog_reload_failed`. `tether_health` remains callable but reports `ok: false`,
`catalog_read.status: "reload_failed"`, and the error instead of stale counts.
The next call retries from disk and recovers as soon as the catalog is valid.
Reload errors contain only bounded `category` and `location` values. Raw decoder
and validator messages are not returned because they can contain configuration
values.

```json
{
  "ok": false,
  "code": "catalog_reload_failed",
  "message": "launch catalog reload failed",
  "error": { "category": "decode", "location": "launches" }
}
```

This read path does not replace the service's startup catalog, reconfigure
provider factories, or mutate daemon-owned sessions. `tether_catalog_refresh`
continues to refresh upstream MCP `tools/list` caches only. Boot-profile reads
already load their directory for each call.

#### `tether_catalog_list_projects`
List all projects defined in the catalog.

#### `tether_catalog_list_agents`
List all agent profiles.

#### `tether_catalog_list_providers`
List all provider definitions.

#### `tether_catalog_list_launches`
List all launch profiles. Each launch combines a project, agent, and provider.

```json
// Response
{
  "ok": true,
  "launches": [
    { "id": "myproject-backend", "project": "myproject", "agent": "backend", "provider": "claude-stream" }
  ],
  "catalog_read": {
    "status": "current",
    "source": "catalog_root",
    "observed_at": "2026-09-13T12:00:00Z",
    "validated": true
  }
}
```

#### `tether_catalog_list_boot_profiles`
List boot-profile YAMLs from `<catalog>/boot-profiles/`. Used with `tether_boot_generate`.

---

### Skills

#### `tether_skill_get`
Load a skill by id and return its instructions. This is the provider-neutral
counterpart to Claude Code's built-in skill loader: when a boot prompt lists a
pointer such as `/refactor-go — Apply Go refactoring patterns`, non-Claude
providers can call `tether_skill_get` with `skill_id: "refactor-go"` and follow the
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

#### `tether_skill_list`
List every skill visible through the same resolver used by `tether_skill_get`.
Returns metadata only; call `tether_skill_get` to load the full body.

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

#### `tether_skill_broker`
Return ranked skill recommendations for a specific task, role, project, or
trigger set. This is the progressive-discovery companion to `tether_skill_list`:
it returns metadata, ranking, and reasons, then the caller uses
`tether_skill_get` only for the chosen skill body.

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
      "next": "tether_skill_get"
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

#### `tether_session_list`
List sessions with optional filtering.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `state` | string | — | Filter: `created`, `running`, `stopped`, `failed` |
| `cursor` | string | — | RFC3339 pagination cursor |
| `limit` | number | — | Max results (default 50, max 200) |

#### `tether_session_get`
Get a single session by ID.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

#### `tether_session_create` _(session.write)_
Create a session from a launch profile. Session starts in `created` state; it
is not running yet. Follow with `tether_session_launch`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `launch_id` | string | ✓ | Launch profile ID from the catalog (see tether_catalog_list_launches) |
| `boot_prompt` | string | — | Optional boot prompt override; replaces catalog static boot fragments verbatim |
| `agent_file` | string | — | v005-08: filesystem path to an agent YAML matching config.Agent shape. Field-merged over the catalog agent. |
| `agent_inline` | string | — | v005-08: JSON-encoded agent definition (same shape as config.Agent). Highest precedence in agent resolve order. |
| `boot_profile` | string | — | v005-08: filesystem path to a bootgen boot-profile YAML. Carries the MCP server allowlist (mcp_servers). |
| `injection` | string | — | Caller-provided JSON config.LaunchInjection (native_files + boot_dir_overlay) supplied outside catalog YAML. Caller native files append after catalog native files; caller boot-dir overlay entries win on duplicate rel_path. SECURITY: persisted at rest in launch_plans — non-secret content only; route secrets through provider env passthrough/whitelist instead. |
| `override` | string | — | v005-08: JSON object applied last over the resolved plan. Fields: system_prompt (string), env (KEY:VAL map). |
| `prompt_append` | string | — | Additional boot-prompt text appended after catalog/agent/override content. Use for narrow launch-time handoffs without replacing the base prompt. |

```json
// Response
{ "ok": true, "session_id": "01abc...", "workspace": "/home/user/.tether/workspaces/...", "log": "..." }
```

#### `tether_session_launch` _(session.write)_
Start a previously created session. Transitions `created → running`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID from `tether_session_create` |

#### `tether_session_stop` _(session.write)_
Send a stop signal to a running session.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

#### `tether_session_wait`
Block until the session exits. Returns the exit code. Read-only; no scope required.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

```json
// Response
{ "ok": true, "session_id": "01abc...", "exit_code": 0 }
```

#### `tether_session_send_input` _(session.write)_
Send raw text to a running session's stdin (PTY). A newline is **not**
appended automatically — include `\n` if you want to submit a command.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |
| `input` | string | ✓ | Text to send |

#### `tether_session_resize` _(session.write)_
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

#### `tether_logical_agent_list`
List all registered logical agents.

#### `tether_logical_agent_resume` _(session.write)_
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
`tether_message_notify.urgency` for delivery urgency: `very-low`, `low`,
`normal`, or `high`.

#### `tether_message_send` _(message.write)_

| Parameter | Type | Required | Description |
|---|---|---|---|
| `from` | string | ✓ | Sender URN |
| `to` | string | ✓ | Recipient URN |
| `kind` | string | ✓ | Message kind |
| `payload_json` | string | — | JSON payload body |
| `thread_id` | string | — | Thread ID for grouping |
| `in_reply_to` | string | — | Message ID this replies to |

#### `tether_message_notify` _(message.write)_
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

#### `tether_message_get`
Get a message by ID. Scoped to the claimed identity — `as` must be the
message's sender or recipient, or the call returns `forbidden` (403).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID |
| `as` | string | ✓ | Caller URN asserting the read (ADR 0045) |

#### `tether_message_inbox`
Pull a recipient's undelivered messages — the atomic-delivery agent pull model.
**Destructive:** what it returns is marked delivered and will not appear in a
future inbox call. For a repeatable browse, use `tether_message_list`.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `to` | string | ✓ | Recipient URN |
| `kind` | string | — | Comma-separated kind filter |
| `thread_id` | string | — | Thread ID filter |

#### `tether_message_thread`
List all messages in a thread, scoped to the ones involving the claimed
identity.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `thread_id` | string | ✓ | Thread ID |
| `as` | string | ✓ | Caller URN asserting the read (ADR 0045) |
| `kind` | string | — | Comma-separated kind filter |

#### `tether_message_consume` _(message.write)_
Mark a message consumed by the recipient.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID |
| `as` | string | ✓ | Recipient URN consuming the message |

#### `tether_message_cancel` _(message.write)_
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
`tether_message_inbox`/`tether_message_list`, `as` on most other reads,
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
Partial-merge update. Scalar fields update column-wise — only fields present in the patch are touched. Array fields follow the patch semantics in the tool's own description; read it via `tether mcp` before relying on replace-vs-append behavior.

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

#### `tether_message_trace`
Full delivery state for one message. Read this before theorising about a message that did not arrive — it distinguishes never-sent from sent-and-unclaimed from delivered-and-ignored, and those have nothing to do with each other.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `message_id` | string | ✓ | Message ID. |

#### `tether_message_retention_candidates`
Messages eligible for a privacy-safe body purge.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `older_than_hours` | number | — | Lookback window in hours; 0 or omitted uses the daemon's default. |

#### `tether_message_redrive` _(delivery.write)_
Re-attempt a stuck or dead-lettered delivery. A repair tool — drive it from trace evidence, not as a retry reflex.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `authorized_by` | string | ✓ | URN recorded as provenance for this repair (self-asserted, ADR 0045). |
| `message_id` | string | ✓ | Message ID, or a literal delivery id for a group-fanout recipient. |
| `new_deadline_seconds` | number | — | New delivery deadline in seconds from now; 0 or omitted means no deadline. |

#### `tether_message_purge` _(delivery.write)_
Clear one message's body and metadata, leaving its structural and trace fields (id, kind, from, to, thread, timestamps) intact. **Irreversible.** Refuses when the message still has a pending delivery obligation — including dead-lettered, which remains repairable via `tether_message_redrive` and would resend an empty message if purged first. Idempotent: purging an already-purged message reports `purged=false` rather than erroring.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `authorized_by` | string | ✓ | URN recorded as provenance for this purge (self-asserted, ADR 0045). |
| `message_id` | string | ✓ | Message ID. |

---

### Agents

Catalog agent profiles. Not messaging — listed here because `catalog.write`
appears in the scope table above.

#### `tether_agent_list`
List agent profiles in the catalog.

_No parameters._

#### `tether_agent_show`
Show one agent profile.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | ✓ | Agent ID |

#### `tether_agent_create` _(catalog.write)_
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

#### `tether_agent_edit` _(catalog.write)_
Edit an existing agent profile. Only fields present in the call are changed.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `id` | string | ✓ | Agent ID to edit. |
| `agent_prompt` | string | — | New persona prompt (optional). |
| `name` | string | — | New human-readable name (optional). |
| `roles` | string | — | Comma-separated role list — replaces existing roles; empty string clears them (optional). |
| `skills` | string | — | Comma-separated skill ID list — replaces existing skills; empty string clears them (optional). |
| `system_prompt` | string | — | New system prompt (optional). |

**Read-only catalog.** Tether write-protects its catalog, run directory and state
directory for the agents it wraps, which is every agent but Codex ([control-plane protection](sandboxing.md#control-plane-protection-every-agent-tether-wraps)),
and starts the `tether mcp` it plants with `--protect-path <dir>` for each. From inside
such an agent, `tether_agent_create` or `tether_agent_edit` that would write under one
(a `system`-scope create, an edit of a catalog agent, or a path that reaches it
through a symlink) returns the typed error `catalog_read_only`, telling the agent to
ask the operator (`tether agents create` / `edit`) or to use `scope=project`, which
writes into the repo. This is a policy of the server, enforced for every runtime,
Codex included (Codex is otherwise not protected, CW-20261001-0230), whether or
not a sandbox also makes the directory read-only, and it is not a raw
read-only-filesystem error. On Linux it holds against a symlink re-pointed while
the call runs: the destination directory is opened once and judged, and the file is
written relative to it without following a symlink. `--protect-path` is repeatable and is set only on a
launched agent's server, never on `tether boot` or your own `tether mcp`.

---

### Boot prompt generation

#### `tether_boot_generate`
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
| `profile_id` | string | ✓ | Boot profile ID (see `tether_catalog_list_boot_profiles`) |

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
tether_catalog_list_boot_profiles   → discover available profiles
tether_boot_generate (profile_id)   → get assembled boot prompt
tether_session_create (launch_id, boot_prompt=<above>)   → create session with it
tether_session_launch (session_id)  → start
```

---

### Observation (durable history)

These tools expose durable history stored in the tether SQLite database plus
a bounded live event wait surface over the daemon SSE stream. All are read-only
and require no auth scope. They are available in both
normal and `--proxy` mode.

> **Note:** `tether_events_tool_calls` (proxy mode only) is now backed by the
> same durable `proxy_events` SQLite table as `tether_proxy_events`. Results
> survive daemon restarts.

#### `tether_session_events`
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

#### `tether_session_checkpoints`
List checkpoints for a session (resolved via its logical agent). Newest first.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

```json
// Response
{ "ok": true, "checkpoints": [...], "count": 2 }
```

#### `tether_session_attachments`
List client attach/detach records for a session. `detached_at` is `""` for
still-open or pre-tracking attachments.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `session_id` | string | ✓ | Session UUID |

```json
// Response
{ "ok": true, "attachments": [{"id":"...","session_id":"...","client_kind":"cli","attached_at":"...","detached_at":"..."}], "count": 1 }
```

#### `tether_proxy_events`
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

#### `tether_events_history`
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

#### `tether_events_wait`
Wait briefly for live daemon or session events from the running `tetherd` event
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
1. tether_catalog_list_launches          → pick a launch_id
2. tether_session_create (launch_id)     → get session_id
3. tether_session_launch (session_id)    → start the agent
4. tether_session_wait  (session_id)     → block until done
```

### Launch with a generated boot prompt

```
1. tether_catalog_list_boot_profiles     → pick a profile_id
2. tether_boot_generate (profile_id)     → get boot_prompt text
3. tether_session_create (launch_id, boot_prompt)
4. tether_session_launch (session_id)
```

### Resume a logical agent from checkpoint

```
1. tether_logical_agent_list             → find the logical_agent_id
2. tether_logical_agent_resume (id)      → starts session with checkpoint context injected
```

### Send a cross-agent message

```
1. tether_message_send (from, to, kind, payload_json)   → get message_id
2. tether_message_inbox (to)            → recipient polls inbox
3. tether_message_consume (message_id, as)              → mark consumed
```

### Inspect or invoke the AI gateway

```
1. tether_ai_list_providers             → discover configured provider ids
2. tether_ai_list_models                → inspect visible models
3. tether_ai_list_routes                → inspect live planner route order
4. tether_ai_route_preview              → preview route/cost without model invocation
5. tether_ai_route_explain              → explain why each route matched or failed
6. tether_ai_chat                       → invoke the gateway and return one final response (requires ai.invoke)
7. tether_ai_chat_stream                → invoke the gateway as a live MCP stream (requires ai.invoke)
8. tether_ai_embeddings                 → generate embedding vectors (requires ai.invoke)
9. tether_ai_usage / tether_ai_budgets     → inspect durable usage and live budget headroom
10. tether_ai_budget_alerts             → inspect durable budget_rejection alerts directly
11. tether_ai_wait_budget_alerts        → wait briefly for live ai.budget_rejected events
12. tether_events_history               → inspect broader durable daemon/session/broker history
13. tether_events_wait                  → reuse the bounded-live pattern for live daemon/session events
14. tether_ai_audit                     → inspect broader durable audit history
```

### AI tool request forms

`tether_ai_route_preview`, `tether_ai_chat`, `tether_ai_chat_stream`, and `tether_ai_embeddings` support two request styles:

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

`tether_ai_chat`, `tether_ai_chat_stream`, and `tether_ai_embeddings` require the `ai.invoke` scope. `tether_ai_usage`,
`tether_ai_budgets`, `tether_ai_budget_alerts`, `tether_ai_wait_budget_alerts` and
`tether_ai_audit` are read-only but require a token (any scope).
`tether_ai_list_providers`, `tether_ai_list_models`, `tether_ai_list_routes`,
`tether_ai_route_preview`, `tether_ai_route_explain`, `tether_events_history` and
`tether_events_wait` are read-only and open.

### AI live streaming

`tether_ai_chat_stream` bridges the daemon `/ai/chat/stream` SSE path into MCP
client notifications while the tool call is still running.

During one streaming invocation, clients can receive:

- `notifications/ai/chat_stream`
  - structured payload with `source: "tether_ai_chat_stream"` and the normalized
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

- **No PTY output streaming.** `tether_session_send_input` sends input; reading
  output requires the daemon's attach endpoint or `go-tether-client`. A
  polling pattern (send input → wait → read session state) works for short
  interactions.
- **In-process SQLite.** Unless it runs with `--daemon-only` (see above), the
  adapter opens its own DB connection. If the daemon is also running, both use
  SQLite WAL mode — concurrent reads work fine; writes serialize at the DB. For
  heavy concurrent write workloads, run the daemon and use `go-tether-client`
  instead.
- **No MCP resources.** Only tools are exposed; MCP resources (for streaming
  file content, etc.) are not yet wired.

---

## Related

- [`docs/adr/0019-mcp-stdio-adapter.md`](adr/0019-mcp-stdio-adapter.md) — design rationale
- [`docs/api/README.md`](api/README.md) — HTTP/UDS daemon API reference
- [`docs/dev-setup.md`](dev-setup.md) — catalog schema and dev workflow
- `go-tether-client` — Go client library for external consumers (`github.com/hollis-labs/go-tether-client`)
- `examples/catalog/boot-profiles/` — example boot-profile YAMLs

## Codex proxy write protection

With control-plane protection enabled on Linux, Tether plants Codex's local
MCP proxy under a protect-only sandbox. Its stdio upstreams and descendants
inherit read-only catalog, run and state directories. Reads, networking, and
writes elsewhere remain available. Claude and OpenCode keep their existing
agent sandbox; this change does not add a second sandbox around their proxies.
If the protected directories cannot be resolved or the wrapper cannot be
constructed, the launch fails; if the OS cannot start the wrapper, the proxy
cannot start. There is no unconfined fallback.

HTTP/SSE upstreams cannot inherit those mounts. The protected Codex proxy skips
them by default, logs `cannot be confined locally`, and reports `status:
excluded` plus the reason through its upstream status/health tools. Other
upstreams still start. An operator can deliberately re-enable a remote upstream
in its catalog YAML:

```yaml
id: tangent
transport: http
url: http://127.0.0.1:8080/mcp
allow_unconfined_remote: true
```

`allow_unconfined_remote` defaults to false and affects protected Codex proxies
only. Opting in trusts the remote server's tools with host-side effects outside
the local sandbox, including possible writes to the control plane; it is logged
at startup. It does not expand the session's upstream grant list.

This protects the planted local process tree, not every Codex configuration or
host service. Codex remains reported as not protected: its dormant per-turn
config guard and caller identity are still pending (CW-20261001-0230 and
CW-20260930-0253). A tampered/replaced MCP configuration or a host service
performing writes on a tool's behalf remains outside this fix.
