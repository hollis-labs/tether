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

## Host and Origin

The remote listener checks the request's exact Host and Origin against `--allowed-host` / `--allowed-origin` in front of every
remote route. The MCP mount runs the same check again, with the same allowed set, before it marks a request remote, so the MCP
handler cannot be reached around it. The MCP SDK's own localhost Host guard is off in the daemon handler (behind a reverse
proxy the connection is loopback and the Host is the public name, which it refused with 403); a local request is checked by the
handler's local authority instead, and the SDK's cross-origin protection stays on. Tests: `TestRemoteMCPMountRepeatsTheListenerAuthority`,
`TestLocalAuthorityReplacesSDKLocalhostGuard`.

## Audit and counters

Each remote MCP session initialize writes one `identity_audit` row: principal, session, and
`remote_mcp_initialize worker=<worker id> node=<Tailscale-User-Login, or ->`. Refusals are counted by reason
(`invalid_token`, `not_bound_agent`, `session_ended`, `session_gone`, `header_too_large`, `identity_unavailable`,
`session_lookup_failed`) in the `remote_mcp_denials` field of `GET /health`.

## Configuration

It needs the daemon's issuer (`Server.Issuer`), the MCP gateway and the launch service. The gateway exists only when
`daemon.mcp_endpoint.enabled: true` (and `modules.local_mcp`); with the endpoint off, `/mcp` stays unavailable remotely and every
remote caller gets the ordinary remote chain's 401 `verified caller identity required`, which looks like a bad token. The daemon
says so instead: at startup (with a remote listener) it logs `remote MCP gate ready` or `remote MCP gate NOT built` with the reason,
and `GET /health` carries `remote_mcp: {available, reason}`. Each gate denial logs one line (`remote MCP denied` with reason, status,
method, session id and node; never the token). `tether doctor` warns when `daemon.remote_listener` is enabled in the catalog while the
endpoint is off. Enabling the endpoint does not change how sessions plant MCP (`daemon.mcp_upstream_ownership` does) and opens no new
listener. The worker side is `tether-worker mcp-http-relay`
(CW-20261016-0160 PR A/B).
