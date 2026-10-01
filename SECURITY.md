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

Agent sessions launched by Tether run under the operator's own uid. On
Linux, every agent Tether launches runs inside a sandbox that makes two of
Tether's own directories read-only for it (CW-20261001-0142):

- the catalog root (`~/.tether/catalog/` by default), so an agent cannot
  rewrite the launch, provider, agent and MCP-server definitions it is run
  from, or the credentials the catalog holds;
- the daemon's run directory (`~/.tether/run/`), which holds `muxd.pid` and
  `muxd.sock`.

Each directory is registered by its real path, so a symlinked `~/.tether` is
covered. The daemon, and anything you run outside an agent, can still write
both. The sandbox needs bubblewrap (`bwrap`) and unprivileged user
namespaces. Where bubblewrap is missing or cannot build a namespace, every
launch Tether must sandbox is refused with 403 `forbidden` naming the fix,
rather than run unprotected. A launch whose work directory, workspace or
state database lies inside a protected directory is refused with 403
`forbidden` too.

**Codex is protected by its own sandbox instead.** Tether does not wrap Codex
in its sandbox, because Codex's own `workspace-write` sandbox is bubblewrap
too and cannot nest inside it where unprivileged user namespaces are
restricted (the Ubuntu default), which would stop Codex running any shell
command. That sandbox makes the filesystem read-only except Codex's working
directories, `/tmp`, `$TMPDIR` and any configured writable root, and the
catalog and run directory are outside them: a Codex turn's `touch <catalog>/x`
is denied (see [`docs/sandboxing.md`](docs/sandboxing.md#codex-uses-its-own-sandbox)).

So that protection is Codex's, not Tether's, and Codex's sandbox can be
widened from several places: its flags, its environment, a `config.toml` in
`CODEX_HOME` or a project `.codex/config.toml`, and its working directory. The
exemption is therefore an **allowlist**. Tether leaves Codex to its own sandbox
only while everything that shapes it is known-safe: the flags are the model and
a short list of overrides that keep the sandbox as it is, the environment has no
`CODEX_*` variable but Tether's own `CODEX_HOME`, nothing is injected into
`CODEX_HOME`, no project `.codex/config.toml` is in the work directory or up to its
project root, and no work directory, workspace or temp directory contains a
protected one. Anything else, or anything it does not recognise, and Tether wraps
Codex like every other agent; where that sandbox cannot start, the launch fails
loudly. Tether cannot see inside Codex's sandbox, so a defect in it would leave
the catalog writable to Codex.

The allowlist is judged at launch, and Codex starts a new process every turn, so it
is **re-checked before each turn** on an exempted session: Codex keeps `.codex`
read-only only from itself, so another agent sharing the project directory, a
checkout or an operator can plant a `.codex/config.toml` (or change the
`CODEX_HOME` `config.toml`) between turns. If one appears, the turn is refused
with 403 `forbidden`, naming the file. This narrows the window and does not close
it: the file could appear between the check and Codex reading its configuration.
Closing it takes caller identity (CW-20260930-0253) and a way to make `.codex`
unwritable to other agents.

Agents run under Tether's sandbox see a private PID namespace, lose
background processes when a per-turn CLI's turn ends, find `/home`, `~` and
`~/.tether` as separate mounts (a `rename` across them fails with `EXDEV`),
and cannot use `sudo`.

On macOS the protection is not applied yet. go-sandbox's seatbelt protection
has not been verified on a real Mac (CW-20261001-0138), so agents there can
still write both directories. The daemon logs a warning at startup and
`mux doctor` reports one.

`mux doctor` asks the running daemon, whose environment decides protection,
and reports a `sandbox-protect` check; `GET /health` carries it as
`sandbox_protect`.

An operator can turn the protection off by setting `TETHER_SANDBOX_PROTECT=0`
(or `false`) in muxd's environment. The daemon then logs a WARN line at
startup, and `mux doctor` reports a `sandbox-protect` warning. Agents can
write the catalog and run directory again, and ACP launches are allowed.
Turning it off is a deliberate decision, never a silent default.

Where it applies, that stops an agent from writing those directories
itself. It does not yet cover:

- **The state database.** This change leaves the state directory writable.
  The `mux mcp` server Tether plants in each agent used to open the state
  database from inside the agent's sandbox, which is why; it no longer does
  (CW-20261001-0173), so the directory can now be protected, in a change of its
  own. Until then an agent can still write it directly.
- **The daemon socket.** A read-only directory does not stop `connect(2)` on
  a Unix socket, so an agent can still call `muxd.sock`. The socket grants the
  full, unauthenticated HTTP API, including writes the daemon makes on the
  caller's behalf. Closing this relies on caller identity: `muxd` verifying
  who is calling (CW-20260930-0253, CW-20260918-0037).
- **Writes delegated to same-uid services.** On Linux the protecting sandbox
  is the host filesystem with the protected directories read-only, not an
  isolation boundary. An agent can still ask a service running as the same
  user to write for it, such as `systemd-run --user`, or the daemon over its
  socket.
- **The user and project layers.** Agent, skill and boot-profile definitions
  under `~/.tether/agents/`, `~/.tether/skills/`, `~/.tether/boot-profiles/`
  and a project's `.tether/` directory are outside the catalog root and stay
  writable (CW-20261001-0192).
- **ACP agents (Copilot, Pi).** go-agent-wrapper's ACP launcher cannot apply
  the protection without a sandbox policy. While protection is on, Tether
  refuses ACP launches with 403 `forbidden` until CW-20261001-0162 adds a
  protect-only sandbox for them.
- **`mux_agent_create` and `mux_agent_edit` from inside an agent.** Planted
  workers still carry the `catalog.write` scope, since it also gates
  `scope=project`, which writes into the repo. A write into the protected catalog
  or `~/.tether/run` fails with a typed `catalog_read_only` error telling the agent
  to ask the operator, not a raw read-only-filesystem error. The scope is not a
  boundary: a worker can start its own `mux mcp --scopes`.

The in-process API stub starts no agent process, so it has nothing to
protect.

**These protections are advisory against a hostile worker until caller identity
lands (CW-20260930-0253).** Any process that can reach `muxd.sock`, a worker
included, can call `POST /sessions` with `agent_inline` (extra flags and
environment per provider), `injection` and an MCP server list, and so shape the
launch it asks for. The Codex allowlist above, and the pin on a catalog agent's
sandbox profile (#85), close what that route can reach today; they do not make the
route itself authenticated.

Until the socket and the state database are covered, treat any agent Tether
launches as able to do anything in Tether that you can do through the
daemon's API.

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
claude.ai connectors (a launched agent has no connectors at all); `TETHER_CLAUDE_STRICT_MCP=0` in muxd's environment turns
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
references (`keychain://…`, `helper://…`, or `file://` for a 0600 file) over
literal values; see [`docs/secrets.md`](docs/secrets.md), including what a
file reference does not hide. The federation registry stores identity
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
- tool-call records from the `mux mcp` Tether plants in an agent (`proxy_events`
  and the `tool_call_*` events in the event log) are asserted by the agent's own
  process. `POST /proxy/events` is the route an agent-side process uses to put
  its own `tool_call_*` events into the event log. The daemon checks the
  record's shape, caps its size, stamps its time and refuses a session that does
  not exist, but it cannot tell which session is calling, so an agent can record
  a call for another existing session (CW-20260930-0253). This is no worse than
  before for the system as a whole, since the agent's own process used to write
  those tables directly, but it is not claimed to be safe as a route: other
  routes, such as `POST /broker/envelopes`, also write events with
  caller-supplied fields
- agents run as the operator's uid. On Linux they cannot write Tether's
  catalog or run directory themselves, but they can write its state database
  and call its socket. On macOS nothing stops them writing any of it yet (see
  "Agents run as your user")
- pre-1.0 contracts and migration guarantees

These are deployment constraints, not hidden roadmap promises. Operate within
them or place Tether behind controls that provide the missing boundary.
