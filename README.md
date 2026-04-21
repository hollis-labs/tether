# Agent Mux

Local-first agent session control plane. Owns session lifecycle, process/PTY
management, sandboxed execution, checkpoint/resume, brokered messaging, and
event streams for any CLI-backed agent (Claude Code, Codex, Kiro, etc.).

## What it is

Agent Mux runs as a per-user daemon (`muxd`) on your machine. Every agent
session — launch, attach, stop, checkpoint — goes through the daemon. Clients
access it over a Unix-domain socket via the `mux` CLI, the TUI, the HTTP API,
or the MCP adapter.

```
mux (CLI) / TUI / MCP client / go-agentmux-client
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

# Stop a session
mux sessions stop <session-id>

# Generate a boot prompt and pipe it to a new session
mux generate-boot nanite.backend.main | pbcopy

# Start the MCP adapter (for LLM tool access)
AGENT_MUX_MCP_TOKEN=your-token \
AGENT_MUX_MCP_SCOPES=session.write,message.write \
mux mcp
```

## MCP adapter

`mux mcp` exposes the runtime as 23 MCP tools over stdio, usable from Claude
Desktop, Claude Code, Cursor, or any MCP-capable agent:

```json
{
  "mcpServers": {
    "agent-mux": {
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
| [`docs/mcp.md`](docs/mcp.md) | MCP adapter setup, auth, tool reference |
| [`docs/api/README.md`](docs/api/README.md) | HTTP/UDS daemon API reference |
| [`docs/sandboxing.md`](docs/sandboxing.md) | Sandbox profiles (macOS + Linux) |
| [`docs/adr/`](docs/adr/) | Architecture decision records (ADR 0001–0019) |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Contribution workflow and code style |

## Catalog

Agent Mux is driven by a YAML catalog at `~/.agent-mux/catalog/`. The catalog
defines projects, agent profiles, providers, and launch configurations. See
`examples/catalog/` for working examples and `docs/dev-setup.md` for the full
schema.

## License

All Rights Reserved — placeholder until v1.0. See `LICENSE`.
