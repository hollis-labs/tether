# MCP restart recovery

The daemon stdio forwarder (`tether mcp --forward-daemon`) and the worker
`tether mcp-http-relay` keep their downstream client connected while replacing a
lost upstream MCP session. Replacement handshakes retry transient connection
failures with delays of 1, 2, 4, 8, 16, then at most 30 seconds, within a 60 second
budget. Caller cancellation and shorter deadlines still apply. Credentials,
profile and discovery selectors retain their existing transport semantics;
HTTP relay bearer files are read again for each replacement attempt.

Explicit `mcp_session_not_found`, `session_expired`, `mcp_initialize_required`
and the SDK's plain `session not found` response identify lost state. An
unrelated route 404 or admission/protocol rejection still surfaces. A replacement
handshake does not grant extra retries to the original operation: it is retried
at most once.

The forwarder probes with ping before tools/call and never repeats a dispatched
tool. The HTTP relay can repeat a request explicitly rejected before dispatch,
or one that failed during dialing. After ambiguous transport loss it restores
the session, but repeats only discovery/ping requests. A tool whose response was
lost still returns an error because its outcome may be unknown.

## Client coverage

- `internal/mcpforward`: UDS/TCP daemon session behind the stdio forwarder. The
  restart regression leaves an SDK consumer connected, closes the real TCP
  server/listener and streams, waits through failed reconnect attempts, then
  binds a fresh SDK server at the same address. Its tool executes once.
- `internal/workerruntime.HTTPRelay`: worker and gateway-launch stdio bridge,
  including the credential-file/run-token gateway spec. A separate regression
  stops and rebinds a real TCP server under a connected stdio consumer.
- Legacy `tether mcp --proxy`/`--daemon-only` builds a local adapter and owns
  catalog upstreams through `internal/mcpadapter.ClientPool`. Its remote
  supervisor and `libs/mcp/plugin-mcp/go-mcp/client` reconnect policy are separate
  from these hub-facing bridges; this change does not modify them.
- Native HTTP clients connecting directly to `/mcp` bypass these bridges. This
  includes any Basalisk/Surface Desk or agent kit configured with native HTTP
  instead of the relay command. Those clients need equivalent bounded
  replacement handshakes in their owning client library. The pinned shared
  `plugin-mcp` v0.10.0 client reconnects synchronously without a backoff budget;
  Tether cannot retrofit that behavior into a different process.

No shared daemon, outpost or gateway restart is needed to run the regression
suite. End-to-end Basalisk, Claude kit and native Codex restart verification must
be coordinated separately with the Lead Orchestrator.
