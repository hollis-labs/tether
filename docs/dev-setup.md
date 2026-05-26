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

## OpenTelemetry

Tether initializes OpenTelemetry tracing through `libs/go-otel`.

Useful env vars:

- `OTEL_EXPORTER_OTLP_ENDPOINT` — collector endpoint, default `localhost:4318`
- `OTEL_SERVICE_NAMESPACE` — Tether sets this to `hollis` unless overridden
- `HOLLIS_OTEL_DISABLED=1` — disable OTEL bootstrap
- `TETHER_OTEL_DISABLED=1` — legacy Tether-specific disable alias

The daemon HTTP surface extracts W3C trace context, and Tether's outgoing
app-to-app HTTP calls inject trace context automatically.

## AI gateway setup

The in-process AI gateway is configured in `global.yaml`. The current schema
is intentionally narrow: enabled providers plus an ordered default route list.

Example:

```yaml
ai:
  policy:
    allow_attachments: false
    max_cost_usd: 0.50
    usage_budget:
      max_cost_usd: 25.00
      window: month
  providers:
    - id: anthropic-work
      type: anthropic
      model: claude-sonnet-4-5
      secret_ref: keychain://anthropic/work
      enabled: true
      policy:
        allow_reasoning: true
        max_output_tokens: 1024
        usage_budget:
          max_cost_usd: 10.00
          scope: caller
    - id: openai-work
      type: openai
      model: gpt-5
      secret_ref: keychain://openai/work
      enabled: false
    - id: llama-local
      type: openai-compatible
      model: llama3.1
      base_url: http://127.0.0.1:11434/v1
      enabled: false
    - id: gemini-work
      type: gemini
      model: gemini-2.5-flash
      secret_ref: keychain://gemini/work
      enabled: false
  routing:
    default_provider_order:
      - anthropic-work
    routes:
      - provider: anthropic-work
        model: claude-sonnet-4-5
        requires_reasoning: true
        allow_tools: false
        max_cost_usd: 0.10
        usage_budget:
          max_cost_usd: 1.00
          window: day
      - provider: openai-work
        model: gpt-5
        mode: summarize
```

Provider notes:

- `anthropic`, `gemini`, and `openai` require `secret_ref` and `model`.
- `openai-compatible` requires `base_url` and `model`; `secret_ref` is
  optional so local unauthenticated servers can work.
- Configured models that are missing from models.dev are still routable. Tether
  synthesizes a conservative text-only catalog entry for those ids so local
  OpenAI-compatible servers can use custom llama/Ollama model names.
- Gemini currently supports text chat, image input, tool declarations,
  streaming chat, and embeddings through the same routed AI gateway surfaces.
- `enabled: true` controls whether the provider is mounted into the daemon.
- `ai.policy` sets global request-shape defaults.
- `providers[].policy` overrides those defaults for one provider.
- Those same policy blocks can also set `max_output_tokens` and `max_cost_usd`
  as inherited planning ceilings.
- `usage_budget` is a separate inherited policy block for durable spend
  governance. It supports `max_cost_usd`, `window` (`day` or `month`), and
  `scope` (`total`, `caller`, or `session`).

Routing notes:

- `routing.routes` is optional. When present, it becomes the planner's ordered
  candidate list.
- Each route names a configured `provider` plus one of that provider's
  configured models.
- `mode`, `intent`, `requires_reasoning`, and `requires_tools` act as match
  constraints for that route.
- `allow_reasoning`, `allow_tools`, and `allow_attachments` are optional
  policy gates. They resolve in this order: route override, then provider
  policy, then global `ai.policy`.
- `max_output_tokens` and `max_cost_usd` resolve in that same order and act as
  hard route policy ceilings before model capability/price evaluation.
- `usage_budget` resolves in that same order, but it is enforced against
  durable `ai_events` chat usage history instead of the current request alone.
  Global budgets aggregate all chat usage, provider budgets aggregate one
  provider across its configured models, and explicit route budgets aggregate
  that exact provider/model pair.
- When one of those resolved values is `false`, the route is considered but
  rejected with an explicit policy error if the request needs that feature.
- `scope: caller` requires `caller_id`; `scope: session` requires
  `session_id`. The planner surfaces those as explicit route-policy failures in
  route explain.
- More specific matching routes are preferred ahead of generic routes, while
  preserving the declared order among equally specific entries.
- If `routing.routes` is omitted, the daemon falls back to
  `default_provider_order` plus each provider's configured model order.

Secrets are resolved at runtime through `mux-apikey-helper`, not stored in
the SQLite state DB. Supported refs include `keychain://...` and
`helper://...`.

Common keychain setup:

```bash
printf '%s\n' "$OPENAI_API_KEY" | mux-apikey-helper set keychain://openai/work
printf '%s\n' "$GEMINI_API_KEY" | mux-apikey-helper set keychain://gemini/work
printf '%s\n' "$ANTHROPIC_API_KEY" | mux-apikey-helper set keychain://anthropic/work
```

Once configured and the daemon is running, the typed AI surfaces are
available through:

- HTTP: `/ai/providers`, `/ai/models`, `/ai/routes`, `/ai/routes/preview`, `/ai/routes/explain`, `/ai/chat`,
  `/ai/chat/stream`, `/ai/embeddings`, `/ai/usage`, `/ai/budgets`, `/ai/audit`
- CLI: `mux ai providers|models|routes|route-preview|route-explain|chat|embeddings|usage|budgets|audit|watch-budgets`
- MCP: `mux_ai_list_providers`, `mux_ai_list_models`,
  `mux_ai_list_routes`, `mux_ai_route_preview`, `mux_ai_route_explain`, `mux_ai_chat`,
  `mux_ai_chat_stream`, `mux_ai_embeddings`,
  `mux_ai_usage`, `mux_ai_budgets`, `mux_ai_audit`

For live alerting instead of polling, `mux ai watch-budgets` subscribes to the
daemon event bus and prints `ai.budget_rejected` events as they arrive. The
underlying stream is `GET /events/stream?scope=daemon&kind=ai.budget_rejected`.

For incremental model output, `mux ai chat --stream "..."` uses
`POST /ai/chat/stream` and prints normalized text deltas as they arrive, then
the final provider/model/usage summary once `response.completed` lands.

For multimodal shorthand, these commands also accept image flags:

- `mux ai chat "describe this" --image-file ./photo.png`
- `mux ai chat "what's in this?" --image-url https://example.com/cat.jpg`
- `mux ai route-preview --image-file ./diagram.png`

Those flags append normalized `image` content parts to the shorthand user
message. Full normalized request JSON/YAML still works when you need more
control over multi-message or mixed-role requests.

More generally, the daemon exposes:

- durable event history via `GET /events`
- live SSE via `GET /events/stream`

The CLI mirrors the durable path as `mux events history`, which supports
repeatable `--scope` and `--kind` filters plus `--session-id`, `--since-seq`,
`--cursor`, and `--limit`.

For live operator tailing, use `mux events watch` with the same `--scope`,
`--kind`, `--session-id`, and `--since-seq` filters. That command streams the
daemon SSE surface directly instead of querying durable history.

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
