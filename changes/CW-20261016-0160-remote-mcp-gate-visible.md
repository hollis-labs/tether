- Remote `/mcp` for worker agents, canary fixes (CW-20261016-0160). The gate is no longer silent when it is not built
  (the hub ran with `daemon.mcp_endpoint.enabled: false`, so bound agent tokens got the generic remote 401): `GET /health`
  gains `remote_mcp: {available, reason}`, startup logs `remote MCP gate ready` / `NOT built` with the reason, each gate
  denial logs a one-line reason (ids only, never the token), and `tether doctor` warns about a catalog remote listener with
  the MCP endpoint off. The MCP SDK's own localhost DNS-rebinding guard is disabled in the daemon MCP handler: behind a
  reverse proxy the connection is loopback and the Host is the public name, which the SDK refused with 403. The local
  Host/Origin authority check and the remote listener's exact allow-lists are unchanged and still decide. Every non-2xx
  from the MCP route now logs one `daemon MCP refused` line with status and reason. See docs/remote-mcp.md.
