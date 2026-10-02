# Connect a client to Tether

Configure one stdio MCP server named `tether`. Tether then connects to the
upstreams in its catalog; the client needs no separate entry for each app.
For upstream setup, use [Add an upstream](add-upstream.md).

## Initialize a new installation

With the installed `tether` on PATH, this creates the starter catalog under
`~/.tether/catalog`:

```sh
tether init --yes
```

For an existing installation, keep its catalog. A different catalog root can
be selected with the global `--catalog` flag; see the [reference](../../mcp.md#quick-start).

## JSON clients

Merge this entry into the client's MCP configuration (Claude Desktop/Code,
Cursor, or another client using `mcpServers`):

```json
{
  "mcpServers": {
    "tether": {
      "command": "tether",
      "args": ["mcp", "--proxy"]
    }
  }
}
```

## Codex

Use this entry in the operator-managed Codex configuration:

```toml
[mcp_servers.tether]
command = "tether"
args = ["mcp", "--proxy"]
```

The client must be able to find `tether` on its PATH; otherwise use its absolute
installed path. These snippets select the default flat mode and all enabled
upstreams. Use [Limit tools](limit-tools.md) to narrow that grant and
[Discovery](discovery.md) to select search mode.

## Confirm the connection

Call `tether_gateway_status` with `{}` in your MCP client. It reports the
resolved mode and upstream connection states. Read-only native tools generally
need no token; mutations require a configured `TETHER_MCP_TOKEN` and appropriate
`TETHER_MCP_SCOPES`. Some AI reads also require a token; see
[authentication and scopes](../../mcp.md#authentication-and-scopes).
Upstream credentials belong in upstream entries, separately from this token.

A launched agent already receives a planted proxy configuration. Use that
configuration; its server grant, daemon-only routing and protections are decided
by Tether at launch. A manually configured client does not acquire those
protections from `--proxy` alone; see [Protection](protection.md).
