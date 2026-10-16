- `tether-worker mcp-http-relay` no longer lets a message overtake the agent's `initialize` (CW-20261016-0160, canary 3): the relay
  forwarded every line concurrently, so a client that sends `notifications/initialized` or a first request right behind
  `initialize` could reach the hub before the session id existed and got `400 mcp_initialize_required`. Messages after an
  initialize now wait for its response. A refusal log line from the relay now names the JSON-RPC method.
