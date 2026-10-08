# Limit upstream tools

Server grants select upstreams before startup and credential resolution. Tool
allowlists filter final wire names in discovery and calls, in both flat and
search modes. Excluded tools cannot be called by guessing their names.

## Restrict an operator's proxy

Here `example` is the catalog ID of an enabled upstream:

```sh
tether mcp --proxy --servers example
```

Native Tether targets remain available. `TETHER_MCP_SERVERS` supplies the list
when the flag is absent; an explicit empty list selects no upstreams:

```sh
tether mcp --proxy --servers ""
```

To omit native targets too:

```sh
tether mcp --proxy --only example
```

Gateway status remains available; search mode also keeps its three discovery
and dispatch tools. `--only` needs its own nonempty list. An unknown or disabled
ID fails startup. Without a selector, an operator's proxy loads every enabled
upstream. `--confine` changes an omitted list to none; it does not itself create
a filesystem sandbox. See [Protection](protection.md).

## Grant servers to a launched agent

For the current catalog launch path, set this fragment in a project YAML:

```yaml
mcp:
  servers: [example]
```

That project grant replaces the default `torque,tesseract`; include those IDs
if the agent needs them. Native tools are separate from upstream grants.
A nonempty project list wins over a launch's `mcp.servers`. A boot profile's
`mcp_servers` applies at session creation; resume re-resolves the project grant
or default, so a boot-only grant does not survive resume. See the
[launch grant reference](../../mcp.md#agents-tether-launches-strict-config-and-an-allow-list).

## Boot tool grants and flat mode

A boot profile can restrict individual tools as well as upstreams:

```yaml
mcp_servers: [torque]
mcp_tools: [torque_task_get, torque_task_list]
```

Omitting `mcp_tools` inherits the existing tool surface; `mcp_tools: []` grants
no target tools. Entries use the gateway profile glob syntax and match final
wire names, after any catalog `tool_prefix`. They do not include a client prefix
such as `mcp__tether__`. Both managed `tether boot` and legacy `boot-exec` carry
this grant. Managed sessions capture it in the launch plan at creation and seal their MCP
policy at launch; a later
profile selection can narrow that policy but cannot broaden it.

The environment form is `TETHER_MCP_TOOLS`, a JSON array, for example
`["torque_task_get","torque_task_list"]`. An absent variable inherits; `[]`
grants no targets. Invalid JSON, `null`, and invalid patterns fail startup.
An operator's local proxy applies the environment grant for that invocation;
it is not a credential boundary for an operator who controls their environment.
Managed session authority comes from the sealed daemon policy.

`tether mcp --no-discover` selects flat mode, equivalent to
`--discovery-mode flat`. It overrides inherited search mode and conflicts with
an explicit `--discovery-mode search`. Flat `tools/list` exposes eligible target
schemas directly, with `tether_gateway_status` retained. Search mode keeps its
three discovery/dispatch tools; all target hydration and calls still obey the
same grants. See [gateway profiles](../../mcp.md) for allow/deny rules,
read-only annotations and ordering.

## Portable Nanite import example

This source-only `.mcp.json` projection requires an enabled `torque` catalog
upstream whose final tool names are `torque_task_get` and `torque_task_list`:

```json
{
  "mcpServers": {
    "tether": {
      "command": "tether",
      "args": ["mcp", "--proxy", "--no-discover", "--servers", "torque"],
      "env": {
        "TETHER_MCP_TOOLS": "[\"torque_task_get\",\"torque_task_list\"]"
      }
    }
  }
}
```

The MCP wire surface is those two targets plus `tether_gateway_status`, subject
to upstream availability. Nanite grants normally use the bare names
`torque_task_get`, `torque_task_list` and `tether_gateway_status`. Its discovery
index can qualify a colliding name as `tether-mux_TOOL`; its author must select
actual discovered registry names through `SyncKnownTools` rather than assuming
a client prefix. Tether's allowlist continues to use unqualified wire names.
A Nanite grant can further restrict the tools but cannot restore a
Tether-excluded target.

The Nanite author owns the tracked projection at
`docs/examples/tether-mcp.json`. Its current `internal/mcpconfig` path is
`Parse` → `ToStoreConfigs` → `Import`: `command`, ordered `args`, and `env`
values are preserved, and an existing server name is skipped rather than
updated. Importing this example therefore does not replace an existing
`tether` record. Updating or adopting a live record is a separate owner action.
The example contains no credentials or task identifiers.
