# Remote MCP for bound agent tokens

A worker's agent reaches the hub's MCP gateway (`/mcp`) over HTTPS with **its own session's issuer agent token**,
instead of `ssh hub tether mcp --daemon-only` (which has no session binding, no saved policy and no capture). The hub's
remote listener stays loopback-only behind Tailscale Serve, with `--allowed-host` enforced; this adds one route to it.

## Who gets in

| Request on the remote listener | Result |
|---|---|
| a bound issuer agent token (JWT; valid signature, not expired, `aud=tether`, `sid` present, `jti` and `sid` not denied) whose session exists and has not ended | the MCP gateway |
| the same, but the session ended or is gone | `401` |
| a JWT with a bad signature, expired, denied, or for another audience | `401` |
| a valid issued token that is not a bound agent token (operator-minted, device, service) | `404` |
| a stored device credential | `404` (unchanged) |
| no credential, a non-JWT credential, the operator token | `401` (unchanged) |
| any other route or `/p/…`, with any credential | exactly as before (an issued JWT gets `401` there) |
| a Host outside `--allowed-host` | `403`, before anything else |

`/p/` is not served remotely. There is no fallback to the device-credential chain for `/mcp`.

The gate matches exactly the path `/mcp` and the MCP methods POST, GET and DELETE, and does not require the
`Tether-Protocol` header: that header is a version gate for the Tether API, not authentication, and MCP clients negotiate
their own version through `initialize` and `MCP-Protocol-Version`. Any other path (`/mcp/anything`, `/mcp/`) and any other
method take the ordinary remote chain and still need the header.

## Same policy as local

The gateway applies the session's saved MCP policy, the authority fingerprint (an issued token pins its claims, not the
token string, so the outpost's in-place refresh continues the session; any change of authority, including narrowed
scopes, is refused), the same limits, and the proxy capture. The view follows the newest verified token. Remote is not a
lighter path.

## Audit and counters

Each remote MCP session initialize writes one `identity_audit` row: principal, session, and
`remote_mcp_initialize worker=<worker id> node=<Tailscale-User-Login, or ->`. Refusals are counted by reason
(`invalid_token`, `not_bound_agent`, `session_ended`, `session_gone`, `header_too_large`, `identity_unavailable`,
`session_lookup_failed`) in the `remote_mcp_denials` field of `GET /health`.

## Configuration

Nothing new: it needs the daemon's issuer (`Server.Issuer`), the MCP gateway and the launch service, all present in a
hub. Without any of them `/mcp` stays unavailable remotely. The worker side is `tether-worker mcp-http-relay`
(CW-20261016-0160 PR A/B).
