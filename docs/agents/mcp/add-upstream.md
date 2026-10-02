# Add an app behind the gateway

An upstream is an app that already serves MCP. Add its entry to the operator's
catalog, then grant its ID to the intended clients. This is manual onboarding;
a wizard that configures profiles, credentials and client snippets is planned
in CW-20261001-0556.

## Register the app

Create `<catalog>/mcp-servers/example.yaml`:

```yaml
id: example
transport: stdio
command: example-mcp
enabled: true
```

`example-mcp` stands for the app's actual MCP executable, available on the
proxy's PATH. Use an absolute executable path if needed; add its required
arguments through `args`. The app must speak MCP over stdio. Tether cannot turn
an arbitrary CLI into an MCP server.

For HTTP/SSE entries and their URL fields, see the
[upstream reference](../../mcp.md). A protected Codex proxy also applies the
[remote opt-in rule](protection.md#remote-upstreams-require-an-explicit-waiver).

## Store a credential by reference

For an upstream using a bearer token, put the token in a private regular file
owned by your user, mode `0600`, then add this field to its entry:

```yaml
token: file://~/example-token
```

This example reads `~/example-token` at connection time. The catalog contains
the reference, not the value. Stdio apps may instead need a reference in `env`
or `args`; follow that app's credential interface. See the
[secret reference rules](../../secrets.md), including symlink limits and the
distinction between authored references and environment expansion.

```sh
tether doctor --json
tether mcp --proxy --servers example
```

Doctor checks enabled file references without displaying their contents or
invoking a credential helper. Once connected, call `tether_gateway_status`
with `{}` and confirm that the `example` origin is connected. Discover its
actual tool names using [Discovery](discovery.md). A catalog entry does not
automatically add this server to launched agents: apply
[their grant](limit-tools.md#grant-servers-to-a-launched-agent) and restart the
client endpoint after configuration changes.
