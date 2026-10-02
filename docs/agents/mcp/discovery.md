# Choose a discovery mode and call a tool

Flat is the default: the client sees native Tether tools and each connected,
granted upstream tool under its real name. Search exposes four tools:
`tether_tool_search`, `tether_tool_list`, `tether_tool_call`, and
`tether_gateway_status`.

## Pick the client surface

Use flat when client permissions or hooks need to match individual tool names.
Use search to keep the initial tool schema inventory small. In search mode,
client permissions and hooks see `tether_tool_call`, so they cannot identify its
downstream target by the client-visible tool name alone. That dispatcher can
perform destructive operations and advertises that fact.

```sh
tether mcp --proxy --discovery-mode flat
tether mcp --proxy --discovery-mode search
```

An environment selector is also supported:

```sh
TETHER_MCP_DISCOVERY_MODE=search tether mcp --proxy
```

Or set this fragment in the catalog's `global.yaml`:

```yaml
mcp:
  discovery_mode: search
```

Selection is CLI flag, then environment, then gateway config, then persisted
Tether setting, then `flat`. Profiles have an intervening mode hook but profile
selection is planned in CW-20260926-0008. Explicit empty or unknown modes are
errors, including invalid values in a lower tier that another tier overrides.
Mode resolves at startup; restart the client endpoint to apply a change.
`--servers` restricts reachability in both modes, as described in
[Limit tools](limit-tools.md).

## Search, hydrate, call

The following blocks are tool arguments, not shell commands. In search mode,
call `tether_tool_search`:

```json
{"query":"tether_session_list"}
```

Read `items` for exact tool names and origins. Search returns summaries by
default; request `detail: "schema"` when you need input schemas in that response.
For a known name, call `tether_tool_list` to get its real schema:

```json
{"names":["tether_session_list"]}
```

Then call `tether_tool_call`, using the schema to construct `arguments`:

```json
{"name":"tether_session_list","arguments":{}}
```

In flat mode, call `tether_session_list` directly with `{}` instead.
For paginated discovery, `next_cursor` continues the same query; `truncated`
means more matches exist. `complete: false` and `unavailable_servers` mean the
inventory is incomplete, not that the unavailable app has no matching tool.
Use [Troubleshoot](troubleshoot.md) for connection and visibility failures.
