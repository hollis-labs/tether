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

The forwarder probes with ping before tools/call and never repeats a tool whose
execution may have occurred. An explicit `503 mcp_stopping` POST rejection is
known to precede dispatch and permits one retry. A stopping GET response can
follow tool execution and does not permit a tool retry. The HTTP relay can repeat a request explicitly rejected before dispatch,
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
  `plugin-mcp` v0.6.0 client reconnects synchronously without a backoff budget;
  Tether cannot retrofit that behavior into a different process.

The Lead Orchestrator's sanitized live trace (mail
`01a1fb0e-8bac-7d58-9170-a5d733a7ad51`) identifies the deployed Basalisk as
Surface Desk, `hollis-labs/surface` commit `b1b526c`, release `v0.4.1`, using
`module/tetherlink.Stdio` and its process pool. Its child command is:

```text
/var/lib/fabric/bin/tether --catalog /var/lib/fabric/tether/catalog --token-file <file> mcp --forward-daemon --daemon-address unix:/var/lib/fabric/tether/run/tetherd.sock
```

That deployed path uses the patched forwarder; the older Basalisk repository is
not the live client. The coordinator owns verification with the actual Surface
Stdio client against an isolated restarted UDS server. The trace establishes
routing coverage; it does not establish that the patched binary is deployed.

No shared daemon, outpost or gateway restart is needed to run the regression
suite. End-to-end Basalisk, Claude kit and native Codex restart verification must
be coordinated separately with the Lead Orchestrator.

The forwarder also negotiates `2025-03-26` for Basalisk-shaped stdio clients.
A raw JSON-RPC regression retains the same downstream connection across a real
upstream restart and checks successful tools/call results before and after it,
with one execution per call. The worker HTTP relay restart regression likewise
uses a 2025-03-26 handshake and tools/call. This establishes bridge behavior; the live child-process trace above confirms
the named deployed client uses that bridge.

Graceful-stop `503 mcp_stopping` responses are recoverable; generic route, auth,
and protocol failures are terminal. Each replacement handshake is capped at
15 seconds within the single 60-second recovery budget. Concurrent HTTP relay
waiters share that budget and its result, and can cancel independently.

Explicit JSON-RPC `mcp_initialize_required` and `session_expired` errors in either
JSON or SSE POST responses enter the same recovery path. SSE interception is
limited to the active request ID before its terminal response has been emitted;
unrelated or standalone stream errors never authorize a tool retry.

Replacement relay sessions become visible only after `notifications/initialized`
is accepted. Transient initialized refusals retry within the same recovery budget.
