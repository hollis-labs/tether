# App-bound seats

An app-bound seat is a normal Tether Claude seat whose static catalog grant
includes the app's MCP endpoint. The app attaches its chat client to that seat
at `/chat/v1`; it supplies app state as the turn's `context` JSON. No MCP
configuration is accepted from the attaching browser.

The [Surface example catalog](../examples/app-bound-seats/catalog) declares:

- `agents/surface-assistant.yaml`: the seat's logical agent identity,
  `surface-assistant`.
- `launches/surface-claude.yaml`: a chat-enabled Claude streaming-stdio launch.
- `projects/surface-chat.yaml`: the workspace and app-only MCP grant.
- `mcp-servers/surface.yaml`: the app's HTTP MCP server at
  `http://127.0.0.1:8080/mcp`, with tool prefix `surface_`.

Copy these entries into your catalog, adjusting the project directory, worker
routing and app URL. The URL must be reachable from the process running the MCP
proxy: localhost on a worker means that worker. Keep credentials in catalog
secret references when the app requires them; do not place tokens in the chat
client or turn context.

The binding uses the existing launch configuration:

```yaml
mcp:
  servers: [surface]
```

Project `mcp.servers` takes precedence over the launch, then the agent. Use a
dedicated project or ensure its grant also includes `surface`. An explicit list
replaces the default upstream list. Resolution sets
`TETHER_MCP_SERVERS=surface`; Claude's generated `.mcp.json` contains the Tether
proxy (`mux`), which loads only the granted Surface HTTP entry. The app is
reached through this proxy rather than a second dynamically injected MCP
server. Change the catalog and relaunch the seat to change its app binding.

Surface must expose only read and act tools through this endpoint. Tether does
not bridge app approvals for MCP tools, so write and confirm tools must remain
unlisted and uncallable at the app's MCP boundary.

## Attach and submit

Start the configured launch using the ordinary daemon launch flow. On a local
throwaway daemon, explicitly enable `daemon.chat_wire.enabled: true` in its
isolated catalog. Use that daemon's `/chat/v1` base URL for the app's shared
chat client. The seat must already have a running Claude session.

1. `POST /chat/v1/agents/surface-assistant/conversation` ensures the conversation.
2. Attach a controller using the returned conversation ID and acquire its lease
   through the shared chat client.
3. Submit the user's text and JSON `context` on the ordinary turn request.

For example, the relevant turn fields are:

```json
{"text":"Summarize this board","context":{"view":"board","board_id":"b1"}}
```

Both Claude streaming-stdio and Claude PTY receive one user message:

```text
<app-context>{"view":"board","board_id":"b1"}</app-context>
Summarize this board
```

Context is compacted, and invalid JSON is refused before delivery. Turns without
context retain their original input. The public conversation event log retains
the user's text, not the prepended context; reconnecting clients resume from
their event cursor without receiving the context block. Context remains visible
to the CLI and its own vendor transcript. An agent can quote context in its
answer, so this delivery boundary is not an output redaction mechanism.

Streaming-stdio advertises deltas; PTY publishes whole transcript parts. Codex
and Agy context delivery are outside this integration. The app's embedded
runner can continue serving its own chat mount independently.

## Verification

`TestWireIntegration` submits private context through the HTTP chat wire,
checks the actual Claude streaming-stdio input, and reads the durable event log
to verify its exclusion. `TestAppContextClaimsSubmittedRun` checks PTY paste and
transcript matching. `TestAppBoundSeatMCPConfig` resolves this example catalog,
plants Claude's MCP config, and checks its grant and Surface HTTP upstream.

A manual integration run additionally needs an authenticated Claude CLI, a
running Surface app exposing `/mcp`, and an isolated Tether daemon on the worker
host. Use the service-start workflow before starting development services;
never use the production agent-os daemon for this check. Verify streamed
deltas, detach/reattach with a cursor, and a Surface read tool call with a
`tool_result` part. These runtime checks are separate from the fixture tests.

## Codex app-server seats

The chat wire also serves local `codex-app-server` (`jsonrpc-stdio`) seats,
including shim-hosted sessions. It copies the existing session's stdout and sends
`turn/start` through its existing thread cache; attaching chat never starts a
second Codex process. Launch or reattach the seat with a daemon that includes
this backend so its stdout tap is installed. `codex exec --json`, PTY Codex and
remote worker seats are not served by this backend. The remote worker provider
protocol currently accepts user frames only and has no `turn/interrupt` route.

Text and reasoning deltas, command execution and MCP tool calls use the shared
`codexappserver` chatstream decoder. Per-turn `context` uses the same private
`<app-context>` block as Claude; echoed user items and RPC responses are excluded
from chat events. Another submission while a turn runs returns busy without
sending input. Cancellation targets that thread and turn with `turn/interrupt`
and ends the chat run with `run.abort`; if the provider does not confirm within
ten seconds, the run aborts locally and further submissions remain busy until the
provider reports the turn completed. Non-chat turns (such as mail wakes) appear
as observed runs. Approval requests appear as activity only; the existing Codex
approval handler remains responsible for answering them. No chat approval bridge
is enabled. `daemon.chat_wire.enabled` still defaults to false.
