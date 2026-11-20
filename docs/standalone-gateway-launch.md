# Standalone MCP gateway in launched sessions

A launch can opt into the standalone gateway with an explicit catalog launch
`env` entry:

```yaml
env:
  TETHER_MCP_GATEWAY_URL: https://mcp.nanite.cloud/mcp
```

The launcher adds an MCP server named `gateway` alongside the existing `tether`
server. Claude's `.mcp.json`, Codex's private `config.toml`, and agy's
`.agents/plugins/tether/mcp_config.json` all use a stdio command:

```text
tether mcp-http-relay --url <endpoint> --token-file <jwt-tmpfs-path> --run-token-file <run-tmpfs-path>
```

On workers the command is `tether-worker mcp-http-relay`. The relay reads the
current bearer for every HTTP request. The launcher copies its sealed
`TETHER_RUN_TOKEN` into a second protected tmpfs file; the relay reads that fixed
run token once and sends `X-Run-Token` on POST, GET and DELETE. Explicit file
handoff survives CLI filtering of stdio-child environment variables. No bearer or run token
is written into the new provider entry or command arguments. This also avoids
agy's literal handling of the environment header forms tested in the run-token
spike. No native HTTP header interpolation or arbitrary environment inheritance is needed.
For agy, endpoint opt-in also enables its existing private-HOME permission file
and grants the two planted `tether` and `gateway` relays. Other plugin servers
remain denied; the operator's global permission file is untouched.

The local launcher mints the gateway JWT in process from its freshly verified
stored session launch credential. This method is not an `/auth/token` grant:
issued JWTs cannot mint a replacement. The original session's narrowed scopes,
revocation and expiry still bound refresh. Gateway JWTs use audience `tether`,
kind `agent`, canonical scopes and the session ID; their maximum lease is 15
minutes. A worker exchanges its paired device credential against its existing
hub-owned session grant. Its device token and the issuer keys stay outside the
session handoff.

Gateway bearer files live in private 0700 directories under Linux `/dev/shm`,
with 0600 files. A narrowed filesystem sandbox receives a read-only mount of
only its own credential directory, preserving visibility across atomic refresh.
Existing deny rules and network restrictions still apply. Each lease refreshes halfway through its actual remaining
lifetime. Failed refreshes retry without extending the old lease; expired files
are removed. Session completion or failed preparation joins the refresh worker
and removes the directory. A restarted worker can resume refresh from its saved
nonsecret path and grant. Local daemon replacement does not reconstruct this
refresh closure; launch a fresh session after a local daemon restart. An exited
or revoked session cannot renew a JWT.

A run-token view rejected with HTTP 401 is retired. Discovery/ping can reconnect
once; a tool call's refusal is surfaced without reposting that call. Subsequent
requests use a replacement view. Other transport failures do not authorize
mutation replay. The relay refuses redirects, URL userinfo and cleartext HTTP
outside loopback.

The gateway still decides upstream visibility through Tether admission and the
saved catalog MCP policy. Opting into the endpoint does not grant an upstream.
A launch with `shared-services` in its grant can discover its `transcript_*`
tools after the gateway operator enables `forward_run_token`; a launch without
that grant cannot acquire them by adding the endpoint.

For rollout, configure one fresh launch first. Upgrade worker binaries before
using the opt-in: the hub requires their `standalone-gateway-v1` capability.
Then verify discovery and one allowed call through the actual standalone
endpoint, and verify an ungranted launch does not see the tools. These live
checks require gateway deployment/configuration and are separate from the
repository's offline tests. This change does not restart services or enable the
gateway's forwarding flag.

The opt-in fails closed without Linux tmpfs, a session agent-token grant, or a
working issuer. Fresh planting is required; ACP and native continuation paths
that reuse existing provider configuration reject the opt-in. Existing provider
login storage and the preexisting Tether connection's credential handoff are
outside this gateway credential mechanism.
