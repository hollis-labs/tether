# Tether

Local-first agent session control plane. Tether owns session lifecycle,
process/PTY management, sandboxed execution, checkpoint/resume, brokered
messaging, and event streams for CLI-backed agents such as Claude Code,
Codex, Kiro, and Opencode.

## What It Is

Tether runs as a per-user daemon (`muxd`) on your machine. Every agent
session, launch, attach, stop, and checkpoint goes through the daemon.
Clients access it over a Unix-domain socket via the `mux` CLI, the HTTP API,
the MCP adapter, the ACP surface, or the Go client library.

```
mux (CLI) / MCP client / HTTP / go-agentmux-client
           │
           ▼
   muxd  (unix socket)
   ├─ /sessions/*      session lifecycle + attach + events
   ├─ /logical-agents/* checkpoint list + resume
   ├─ /messages/*      cross-agent messaging (go-messaging)
   ├─ /broker/*        typed envelope delivery
   ├─ /catalog/*       read-only catalog projection
   └─ /events/*        SSE event bus
```

## License & Branding

Tether is open source under the MIT License.

You are free to use, modify, and build on Tether for personal or commercial
use.

The Tether name and Hollis Labs branding are protected trademarks. If you
build on Tether, describe that relationship in a way that does not imply your
fork or service is the official Tether distribution.

See [TRADEMARK.md](TRADEMARK.md) for details.

## Build

```bash
make build          # produces bin/mux and bin/muxd
make check          # fmt + vet + lint + test-race + vuln
```

Requires Go 1.26+. See [`docs/dev-setup.md`](docs/dev-setup.md) for the
full development setup including catalog configuration.

## Quick start

```bash
# Start the daemon
mux daemon start

# Launch an agent session from a catalog launch profile
mux sessions launch myproject-backend

# Attach to a running session
mux sessions attach <session-id>

# List sessions
mux sessions list

# Send durable mail, or notify + wake a live recipient session
mux messages send --from msg://user/local/me --to msg://agent/local/worker "hello"
mux messages notify --from msg://user/local/me --to msg://agent/local/worker --urgency high "check inbox"

# Stop a session
mux sessions stop <session-id>

# Generate a boot prompt and pipe it to a new session
mux generate-boot nanite.backend.main | pbcopy

# Start the MCP adapter (for LLM tool access)
AGENT_MUX_MCP_TOKEN=your-token \
AGENT_MUX_MCP_SCOPES=session.write,message.write \
mux mcp
```

## Sysop GUI

Tether ships with Sysop, the GUI for operating the runtime.

```bash
cd apps/sysop
make all
./tether_sysop
```

Sysop serves the UI and API at `http://localhost:8947/operations/`. See
[`apps/sysop/README.md`](apps/sysop/README.md) for development and packaging
details.

## MCP adapter

`mux mcp` exposes the runtime as 23 MCP tools over stdio, usable from Claude
Desktop, Claude Code, Cursor, or any MCP-capable agent:

```json
{
  "mcpServers": {
    "tether": {
      "command": "mux",
      "args": ["mcp"],
      "env": {
        "AGENT_MUX_MCP_TOKEN": "your-token",
        "AGENT_MUX_MCP_SCOPES": "session.write,message.write"
      }
    }
  }
}
```

See [`docs/mcp.md`](docs/mcp.md) for the full tool reference and setup guide.

## Documentation

| Doc | Contents |
|---|---|
| [`docs/dev-setup.md`](docs/dev-setup.md) | Full dev setup, catalog schema, common tasks |
| [`docs/messaging.md`](docs/messaging.md) | Direct mail, notify+wake, inbox/list semantics |
| [`docs/mcp.md`](docs/mcp.md) | MCP adapter setup, auth, tool reference |
| [`docs/api/README.md`](docs/api/README.md) | HTTP/UDS daemon API reference |
| [`docs/sandboxing.md`](docs/sandboxing.md) | Sandbox profiles (macOS + Linux) |
| [`docs/adr/`](docs/adr/) | Architecture decision records (ADR 0001–0019) |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Contribution workflow and code style |

## Catalog

Tether is driven by a YAML catalog at `~/.tether/catalog/`. The catalog
defines projects, agent profiles, providers, and launch configurations. See
`examples/catalog/` for working examples and `docs/dev-setup.md` for the full
schema.
