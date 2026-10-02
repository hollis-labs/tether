# Limit upstream tools

Current filtering selects whole upstream servers. It applies to startup,
credential resolution, discovery and calls, in both flat and search modes.
An excluded server is not a hidden callable inventory.

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

## Planned tool-level profiles

CW-20260926-0008 is implementing profiles with tool allow/deny patterns,
`read_only` filtering and deterministic ordering. Profile selection is not yet
available in the shipped binary described here. Current native scope checks
are described in [Connect](connect.md#confirm-the-connection); per-agent quotas,
rate limits and approval-required tools are planned in CW-20261001-0555.
