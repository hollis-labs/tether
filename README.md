# Tether

Tether is the local-first agent session control plane. A per-user daemon
(`muxd`) owns session lifecycle, process/PTY management, sandboxed execution,
checkpoint/resume, brokered messaging, and event streams for CLI-backed
agents such as Claude Code, Codex, Kiro, and OpenCode. Clients reach it over
a Unix-domain socket through the `mux` CLI, the HTTP API, the MCP stdio
adapter, the ACP surface, or `go-tether-client`.

> **Pre-release.** Tether is under active internal development and already
> runs as the daily session/messaging substrate for the Hollis Labs agent
> fleet — it is not experimental idle code. It has no public release, no
> tagged binaries, and no outside consumers yet, and interfaces still change
> without notice. Built in the open, documented as it stands today.

## What it is today

- **Session control plane.** Every launch, attach, stop, and checkpoint for a
  CLI agent goes through `muxd`; clients never manage the process directly.
- **Brokered messaging.** Durable mail (`mux messages send`) and
  notify+wake (`mux messages notify`) between agents and humans, plus typed
  envelope delivery through `/broker/*`.
- **Event streams.** Durable event history and an SSE bus (`/events*`,
  `mux events watch`) for daemon and session activity.
- **MCP adapter, both directions.** `mux mcp` exposes the runtime as MCP
  tools over stdio for any MCP client (Claude Desktop, Claude Code, Cursor);
  Tether is also the MCP connection this assistant is running on right now —
  every `torque_*`, `tesseract_*`, `tangent_*`, `cerberus_*`, and `loom_*`
  tool call in this session arrives over one `mux` MCP endpoint.
- **AI gateway.** A typed, multi-provider chat/embeddings surface
  (Anthropic, Gemini, OpenAI, OpenAI-compatible) with routing rules, request
  budgets, and durable usage accounting — reachable over HTTP, CLI, or MCP.
- **Sysop GUI.** `tether_sysop`, bundled in every build, gives a local
  operations view over sessions, MCP, AI routing, activity, and the registry.

## Where it sits in the stack

```
   you / agents / other apps      Claude Code, Codex, OpenCode, Torque,
         │                        Tesseract, Nanite — anything with an
         ▼                        MCP client or the mux CLI/HTTP API
   ┌───────────┐
   │  muxd     │   session lifecycle, messaging, events, AI gateway,
   │ (Tether)  │   sandboxing — the substrate, not the orchestrator
   └───────────┘
         │
   launched sessions      the actual coding-agent processes Tether
                           supervises (PTY, checkpoint, recovery)
```

Tether doesn't decide what a task means or what an agent should do next —
that's Torque's and each agent's job. It owns getting a session running,
keeping it addressable, and moving messages and events between whatever is
attached.

## Examples

**Daily driver.** Chrispian launches and attaches to CLI agent sessions
through `mux sessions launch` / `mux sessions attach`, and this Claude Code
session itself is reached through Tether's MCP adapter.

**Composition.** An agent running under Tether calls out to Torque and
Tesseract as ordinary MCP tools over the same connection, sends durable mail
to another agent via `mux messages send`, and gets notified back through
`mux messages notify` when a reply lands — no polling, no direct
process-to-process coupling.

**Cross-host.** `docs/messaging-federation.md` covers routing messages to a
peer Tether daemon on another host, for agents that aren't all running on
the same machine.

## Roadmap

- **Platform reshape to a directory-registry substrate.** Moving from
  precomputed per-project/agent/provider launch files to a live directory
  service that consumers register capabilities with (agent sources, skill
  sources, MCP servers, execution templates) and a parameterized launch
  engine (`go-agent-launch`) that resolves them at call time. Ships alongside
  the current catalog system so existing launches keep working during the
  migration.
- **Boot Assembly Spec.** Replacing static `launches/` and `boot-profiles/`
  files with a single parameterized artifact per agent, resolved against
  live inputs instead of a frozen file per project × agent × provider tuple.
- **Foundation completion for downstream consumers.** Hardening
  provider/session contracts, message routing, and agent/skill/boot
  specification so Nanite and other systems can build on Tether as an
  opt-in substrate rather than a bespoke integration.

## License & Branding

Tether is open source under the MIT License.

You are free to use, modify, and build on Tether for personal or commercial
use.

The Tether name and Hollis Labs branding are protected trademarks. If you
build on Tether, describe that relationship in a way that does not imply your
fork or service is the official Tether distribution.

See [TRADEMARK.md](TRADEMARK.md) for details.

## Install

Tether ships as two binaries:

- `mux` — the main CLI and daemon launcher
- `mux-apikey-helper` — optional helper for local keychain-backed AI secrets

Install paths:

### Option 1: Homebrew

Once the tap formula is published:

```sh
brew install hollis-labs/tap/tether
```

### Option 2: Release tarball

Once tagged releases are published:

```sh
curl -L -o tether.tar.gz \
  https://github.com/hollis-labs/tether/releases/download/v<version>/tether_<version>_darwin_arm64.tar.gz
tar -xzf tether.tar.gz
install -d "$HOME/.local/bin"
install -m 0755 mux "$HOME/.local/bin/"
install -m 0755 mux-apikey-helper "$HOME/.local/bin/"
export PATH="$HOME/.local/bin:$PATH"
```

### Option 3: Build from source

```sh
git clone git@github.com:hollis-labs/tether.git
cd tether
make build
export PATH="$PWD/bin:$PATH"
```

Or install into a prefix:

```sh
make install PREFIX="$HOME/.local"
```

### Option 4: `go install`

```sh
go install github.com/hollis-labs/tether/cmd/mux@latest
go install github.com/hollis-labs/tether/cmd/mux-apikey-helper@latest
```

After install, run `mux init` for the guided first-time setup (idempotent,
skippable at every step). See [`docs/install.md`](docs/install.md) for
prerequisites, keychain configuration, and path details.

## Build

```bash
make build          # produces bin/mux and bin/mux-apikey-helper
make check          # fmt + vet + lint + test-race + vuln
```

Requires Go 1.26+. See [`docs/dev-setup.md`](docs/dev-setup.md) for the
full development setup including catalog configuration.

## Quick start

```bash
# Guided first-time setup (idempotent; detects claude/codex/opencode automatically)
mux init

# Start the daemon
mux daemon start

# Check detected provider binaries and system health
mux detect
mux doctor

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

# Inspect configured AI providers
mux ai providers

# Inspect durable event history
mux events history --scope daemon --limit 20

# Stream live events
mux events watch --scope daemon --kind daemon.started

# Preview or invoke the AI gateway
mux ai route-preview "Summarize this diff"
mux ai chat "Summarize this diff"
mux ai chat --stream "Summarize this diff"
```

## Sysop GUI

`tether_sysop` is the operations GUI, bundled in every release tarball and the
Homebrew formula. Start it after `mux daemon start`:

```bash
tether_sysop
# → http://localhost:8947/
```

The Operations page is the home landing; direct routes include `/overview`,
`/mcp`, `/ai`, `/activity`, `/tools`, `/registry`, `/logs`, `/settings`.

To build from source:

```bash
cd apps/sysop
make all
./tether_sysop
```

See [`apps/sysop/README.md`](apps/sysop/README.md) for development and packaging
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

## AI gateway

Tether also exposes a typed local AI gateway when `global.yaml` configures
at least one enabled AI provider. Supported provider types currently include
`anthropic`, `gemini`, `openai`, and `openai-compatible`. The current surfaces are:

- HTTP: `/ai/providers`, `/ai/models`, `/ai/routes`, `/ai/routes/preview`, `/ai/routes/explain`, `/ai/chat`,
  `/ai/chat/stream`, `/ai/embeddings`, `/ai/usage`, `/ai/budgets`, `/ai/audit`
- CLI: `mux ai providers|models|routes|route-preview|route-explain|chat|embeddings|usage|budgets|audit|watch-budgets`
- MCP: `mux_ai_list_providers`, `mux_ai_list_models`,
  `mux_ai_list_routes`, `mux_ai_route_preview`, `mux_ai_route_explain`, `mux_ai_chat`,
  `mux_ai_chat_stream`, `mux_ai_embeddings`,
  `mux_ai_usage`, `mux_ai_budgets`, `mux_ai_audit`

`mux ai chat` and `mux ai route-preview` accept either simple text input or a
full normalized request via `--request-file` or `--request-json`. `mux ai chat
--stream` uses the daemon SSE surface and renders incremental text deltas plus
the final normalized response summary.
For multimodal shorthand, `mux ai chat`, `mux ai route-preview`, and
`mux ai route-explain` also accept `--image-file` and `--image-url` to append
image parts without hand-writing normalized JSON.
`mux ai embeddings "hello world"` generates vectors through the same routed
gateway, and configured custom local model ids remain routable even when
models.dev does not yet know them.
Operators can also define ordered `ai.routing.routes` entries in
`global.yaml` to steer provider/model selection by mode, intent, reasoning,
or tool requirements, plus optional `allow_*` policy gates that reject
disallowed request shapes with explicit route-explain diagnostics. Those
policy gates, request-local budget ceilings, and durable `usage_budget`
controls can be set globally, per provider, or per route. Durable usage
budgets are enforced from the `ai_events` history with daily or monthly
windows and total, caller, or session scope. `mux ai watch-budgets` tails
live `ai.budget_rejected` daemon events over
`/events/stream?scope=daemon&kind=ai.budget_rejected`.

Keychain setup examples:

```bash
printf '%s\n' "$OPENAI_API_KEY" | mux-apikey-helper set keychain://openai/work
printf '%s\n' "$GEMINI_API_KEY" | mux-apikey-helper set keychain://gemini/work
printf '%s\n' "$ANTHROPIC_API_KEY" | mux-apikey-helper set keychain://anthropic/work
```

## Documentation

| Doc | Contents |
|---|---|
| [`docs/dev-setup.md`](docs/dev-setup.md) | Full dev setup, catalog schema, common tasks |
| [`docs/messaging.md`](docs/messaging.md) | Direct mail, notify+wake, inbox/list semantics |
| [`docs/messaging-adoption.md`](docs/messaging-adoption.md) | How an app, agent or hand-run MCP session joins messaging |
| [`docs/messaging-integration.md`](docs/messaging-integration.md) | Implementer's guide: identity ownership, binding lifecycle, error handling |
| [`docs/messaging-federation.md`](docs/messaging-federation.md) | Cross-host peer routing and trust |
| [`docs/workstreams.md`](docs/workstreams.md) | Workstreams, session refs, and the recovery digest |
| [`docs/mcp.md`](docs/mcp.md) | MCP adapter setup, auth, tool reference |
| [`docs/api/README.md`](docs/api/README.md) | HTTP/UDS daemon API reference |
| [`docs/go-client-migration.md`](docs/go-client-migration.md) | Migrating apps from `go-agentmux-client` to `go-tether-client` |
| [`docs/sandboxing.md`](docs/sandboxing.md) | Sandbox profiles (macOS + Linux) |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | Contribution workflow and code style |

Architecture decision records and internal planning history moved out of the
repo in a docs cleanup pass — see `AGENTS.md` for where.

## Catalog

Tether is driven by a YAML catalog at `~/.tether/catalog/`. The catalog
defines projects, agent profiles, providers, and launch configurations. See
`examples/catalog/` for working examples and `docs/dev-setup.md` for the full
schema.
