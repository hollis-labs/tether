# Tether Developer Setup

Guide to getting a working development environment for Tether.

## Prerequisites

- **Go** — version matching [`go.mod`](../go.mod). The `toolchain`
  directive auto-downloads the minor patch if your local Go is older
  (needs `GOTOOLCHAIN=auto`, which is the default).
- **Make** — BSD or GNU, either works.
- **Git** — for repo operations.
- **A Unix-like OS** — macOS and Linux are tested. Windows is not
  currently supported (the daemon uses Unix domain sockets by default).

## First-time setup

```bash
git clone <repo-url>
cd tether

# Install lint / vuln / test tooling into $GOBIN (usually ~/go/bin).
# Add ~/go/bin to PATH if it isn't already.
make tools-install

# Build the binary into bin/mux.
make build

# Run the full gate to confirm the environment is clean.
make check
```

`make tools-install` pins:

- `golangci-lint` v2 (lint)
- `govulncheck` (vulnerability scan against the Go vulnerability database)
- `gotestsum` (readable test output + coverage collection)

## Layout at a glance

```
cmd/mux/          CLI entrypoint (Cobra) + daemon sub-commands
internal/
  agent/          LogicalAgent type (durable agent identity)
  api/            HTTP handlers + typed error envelope
  app/            Composition root (thin)
  broker/         Broker envelope service + types
  checkpoint/     Checkpoint types
  client/         HTTP/UDS client for the daemon API
  config/         Catalog loader + validation
  daemon/         Daemon server, PID file, listener, lifecycle
  events/         Event bus + Publisher / Persister / Filter
  launch/         Launch plan resolution + boot-prompt composition
  provider/       Provider Runtime + Session interfaces
    api/stub/     No-op API-backed runtime (echo)
    cli/claudestream/  Shared go-agent-sessions runtime for Claude adapters
  runtime/        Session registry + attach broker + lifecycle manager
  session/        PTY handle + Start primitive
  store/          SQLite storage (pure-Go, modernc driver) + migrations
  workspace/      Per-session workspace materialization
docs/
  adr/            Architecture Decision Records
  api/            HTTP/UDS API reference
  dev-setup.md    This file
examples/catalog/ Minimal catalog used by tests + demos
```

## The daemon at a glance

Tether splits into two parts:

1. **`mux daemon`** — a long-lived process that owns running sessions,
   exposes a local HTTP API over a Unix domain socket, and publishes
   lifecycle / broker events.
2. **`mux ...` (other commands)** — a thin CLI that talks to the daemon
   over the socket, with read-only SQLite fallbacks when the daemon is
   down.

Default transport: `unix:~/.tether/run/muxd.sock`. Override via the
`daemon.listen_addr` field in `~/.tether/catalog/global.yaml`.

## Running a demo launch

The `examples/catalog/` tree has a minimal profile that spawns a no-op
API stub — useful for exercising the daemon without installing any real
CLI.

```bash
# Create a throwaway catalog dir.
export AGENT_MUX_CATALOG=$(mktemp -d)
cp -R examples/catalog/* "$AGENT_MUX_CATALOG/"

# Start the daemon.
./bin/mux daemon start --catalog "$AGENT_MUX_CATALOG"
./bin/mux daemon status --catalog "$AGENT_MUX_CATALOG"

# Create + launch a demo session against the api-stub provider.
./bin/mux launch --catalog "$AGENT_MUX_CATALOG" --launch api-stub-launch

# List sessions.
./bin/mux sessions list --catalog "$AGENT_MUX_CATALOG"

# Stop the daemon (also stops all sessions it owns).
./bin/mux daemon stop --catalog "$AGENT_MUX_CATALOG"
```

All artifacts (SQLite state DB, session workspaces, logs, PID file,
socket) land under `$AGENT_MUX_CATALOG` so cleanup is `rm -rf`.

## Iterating during development

- `make test` — fast compile + test cycle, no race detector, with
  coverage. Use this while writing code.
- `make test-race` — correctness gate with the race detector. Slower;
  run before committing.
- `make lint` — full golangci-lint. Config in `.golangci.yml`.
- `make vuln` — govulncheck against stdlib + modules.
- `make check` — the full gate. Matches CI exactly.
- `make coverage` — prints the total coverage percentage from the last
  `make test` run.
- `make coverage-html` — opens the HTML coverage report in the browser.

## Common troubleshooting

### "daemon already running" but no process is alive

The PID file is stale. If `mux daemon status` reports "stale PID",
`mux daemon start` will clear it and proceed; otherwise delete the
pidfile manually:

```bash
rm ~/.tether/run/muxd.pid
```

### SQLite "no such column" after editing migrations

The migrations are `//go:embed`'d into the `mux` binary. A stale
binary (built before you added a new `.sql`) will silently skip the
new migration, so `schema_migrations` drifts from the catalog state
DB. Rebuild:

```bash
make build
```

Then restart the daemon.

### `govulncheck` fails with stdlib vulns

Usually the fix is a newer Go patch release. Check the `toolchain`
directive in `go.mod` — bumping it to the patch that fixes the issue
(e.g., `toolchain go1.26.2`) will pull the fixed stdlib on the next
build. The `GOTOOLCHAIN=auto` default downloads it automatically.

### Claude CLI provider can't find `claude`

The `claude-code` and `claude-stream` providers launch the real `claude` CLI. If you don't
have it installed, use the `api-stub-launch` profile for demos; real
launches need `claude` on `$PATH`.

### Race detector catches a flake

Race failures are always investigated — do not mark them as flakes.
Re-run with `go test -race -count=5 -run TestName ./internal/runtime`
to confirm reproducibility, then fix the synchronization. The runtime
manager is the usual suspect.

## MCP adapter

`mux mcp` starts an MCP stdio server so LLM-based tools can call Tether
capabilities as tool calls. It connects directly to the catalog (no daemon
required) and gates mutating operations behind a token + scope.

```bash
# Read-only access (no auth):
mux mcp

# With mutating tool access:
AGENT_MUX_MCP_TOKEN=dev-token \
AGENT_MUX_MCP_SCOPES=session.write,message.write \
mux mcp
```

Add to Claude Desktop / Claude Code:

```json
{
  "mcpServers": {
    "tether": {
      "command": "/path/to/bin/mux",
      "args": ["mcp"],
      "env": {
        "AGENT_MUX_MCP_TOKEN": "your-token",
        "AGENT_MUX_MCP_SCOPES": "session.write,message.write"
      }
    }
  }
}
```

See [`docs/mcp.md`](mcp.md) for the full tool reference, boot-profile setup,
and cross-agent messaging examples.

## Related docs

- [`docs/mcp.md`](mcp.md) — MCP adapter setup and tool reference.
- [`docs/api/README.md`](api/README.md) — HTTP/UDS API reference.
- [`docs/adr/`](adr/) — architecture decision records.
- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — contribution workflow + code style.
