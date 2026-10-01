# Security policy

## Supported versions

Tether is pre-1.0 software. Security fixes are made on `main` and the newest
tagged release. Older releases may not receive backports.

## Report a vulnerability

Do not include an exploit, token, API key, session transcript, database, or
other sensitive material in a public issue.

Use GitHub's private vulnerability-reporting flow when the repository's Security
tab offers it. If it is unavailable, contact a repository maintainer privately
through a contact channel published on the Hollis Labs organization or
maintainer profile. Include:

- the affected commit or version and operating system
- how `muxd` was configured (Unix socket or TCP listener, MCP adapter flags)
- reproduction steps and the security impact
- whether credentials or user data may have been exposed
- a safe way to contact you about coordination

Maintainers will acknowledge a private report, investigate it, and coordinate
disclosure; response times are best effort during the pre-release period.

## Deployment boundary

Tether is a **single-user, same-host** control plane. The `muxd` HTTP API has no
authentication: trust is anchored to the filesystem permissions of the Unix
domain socket (default `~/.tether/run/muxd.sock`). Anyone who can connect to
that socket can create, steer and stop sessions, send messages and read
session output as the daemon's user.

- Keep the socket and its parent directory owner-only.
- If you set `daemon.listen_addr` to a TCP address, use a loopback address
  (`tcp:127.0.0.1:PORT`). Do not bind `muxd` to a non-loopback interface; there
  is no TLS and no bearer-token check on the HTTP API.
- Caller identity on messaging and group routes (the `as` / `from` URN) is
  provenance supplied by the caller, not an authenticated credential.

The MCP stdio adapter (`mux mcp`) is the one surface with scopes. Read-only
tools need no token. Mutating tools require a token and the matching scope
(`session.write`, `message.write`, `registry.write`, `groups.write`,
`delivery.write`, `catalog.write`, `ai.invoke`); scopes are per capability
group, not a hierarchy. See [`docs/mcp.md`](docs/mcp.md). The token is a
local guard for the client you configure, not a tenant-isolation boundary: run
the adapter only for clients you trust with the daemon.

## Sandboxing

Sessions can run under a sandbox profile ([`docs/sandboxing.md`](docs/sandboxing.md)).
On macOS this uses `sandbox-exec` with a default-allow, selective-deny posture
(outbound network and sensitive filesystem paths are blocked; other access is
permitted). `sandbox-exec` is deprecated by Apple. Treat the sandbox as a
containment aid, not a hardened boundary against a hostile agent.

## Agents run as your user

Agent sessions launched by Tether run under the operator's own uid. Today
nothing stops an agent from reading or writing Tether's control-plane state:

- the catalog under `~/.tether/catalog/`, including credentials it holds;
- the state database (its path is `defaults.state_db` in
  `~/.tether/catalog/global.yaml`);
- the daemon socket `~/.tether/run/muxd.sock`, which grants the full,
  unauthenticated HTTP API;
- the MCP token in a client's config, such as the `mux` entry in
  `~/.claude.json`, and the one planted in each worker's `.mcp.json`.

The MCP token and scopes are checked inside the agent's own `mux mcp`
process; `muxd` does not verify them. An agent that calls the socket directly
bypasses them. Sandbox profiles do not yet deny these paths.

Two planned changes close this:

- CW-20260930-0237: agent sandbox profiles deny access to control-plane state
  and credentials.
- CW-20260930-0253: `muxd` verifies a per-caller identity, so an agent acts as
  itself, not as the operator.

Until both land, treat any agent Tether launches as able to do anything in
Tether that you can.

One interim guard is in place for the sandbox: a session-create override
(`agent_file` or `agent_inline`) may not change a catalog agent's pinned
sandbox profile. It may only tighten an agent that has none. An agent holding
`session.write` therefore cannot loosen its own sandbox by creating a child
session through the API. The guard is removed when CW-20260930-0253 lands and
the daemon can tell an operator from an agent. It does not stop an agent from
editing the catalog itself; that is CW-20260930-0237.

Two defaults narrow which MCP servers a launched agent is handed
(CW-20261001-0227). Claude agents run with `--strict-mcp-config`, so they do not
inherit servers from your `~/.claude.json`, project `.mcp.json` files or the
claude.ai connectors; `TETHER_CLAUDE_STRICT_MCP=0` in muxd's environment turns
this off, and muxd then warns at startup and `mux doctor` warns. An agent's
`mux` proxy is confined to an allow-list of upstreams, by default `torque` and
`tesseract`. `cerberus` is never in the default. These stop an agent from being
handed a server by accident. They do not stop one that goes looking: an agent
can create a child session through the API with a wider list, or run an
upstream's binary itself, until CW-20260930-0253 and CW-20260930-0237 land.

## Data at rest

Tether has no built-in at-rest encryption. The state database, session logs,
checkpoints, message history and catalog files live under `~/.tether/` and
`~/tether/` (the state DB location is set by `defaults.state_db` in
`~/.tether/catalog/global.yaml`) and may contain prompts, agent output and
other sensitive content. Protect them with normal user-account and disk
encryption controls.

Catalog YAML can hold credentials in plaintext (for example
`resources[].config.env` and MCP server `env:`/`token:` fields). Prefer secret
references (`keychain://…`, `helper://…`) over literal values; see
[`docs/secrets.md`](docs/secrets.md). The federation registry stores identity
and a callback URI only, and deliberately does not cache catalog payloads.
Do not commit catalog files containing real tokens.

## External data processors

Tether launches third-party agent CLIs and, when configured, calls model
providers through its AI gateway (OpenAI, Anthropic, Gemini, Ollama-compatible
endpoints and others). Prompts, session content and embeddings input sent
through those runtimes and providers are governed by those providers' terms,
not Tether. Optional OpenTelemetry export is configured through the process
environment and sends trace data to the endpoint you choose.

## Current security limitations

- no authentication on the daemon HTTP/UDS API
- no built-in TLS; TCP listeners are loopback-only by convention
- no at-rest encryption of state, logs or backups
- macOS sandboxing relies on a deprecated mechanism with a default-allow posture
- MCP scopes are coarse capability guards, not multi-tenant isolation
- caller URNs on messaging routes are unauthenticated provenance
- agents run as the operator's uid and can read and write Tether's catalog,
  state database, socket and MCP token (see "Agents run as your user")
- pre-1.0 contracts and migration guarantees

These are deployment constraints, not hidden roadmap promises. Operate within
them or place Tether behind controls that provide the missing boundary.
