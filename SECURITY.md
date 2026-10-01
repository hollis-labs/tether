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
Linux, Claude, OpenCode and every other agent Tether launches, **except
Codex**, runs inside a sandbox that makes three of Tether's own directories
read-only for it (CW-20261001-0142, CW-20261001-0173):

- the catalog root (`~/.tether/catalog/` by default), so an agent cannot
  rewrite the launch, provider, agent and MCP-server definitions it is run
  from, or the credentials the catalog holds;
- the daemon's run directory (`~/.tether/run/`), which holds `muxd.pid` and
  `muxd.sock`;
- the directory holding the state database (`~/.tether/state/` by default),
  where every session, message and event is kept. An agent Tether wraps cannot
  write the database, or replace it or its WAL files. The `mux mcp` server
  Tether plants in each agent never opens it: that server runs `--daemon-only`
  and reaches Tether's state only through the daemon.

Each directory is registered by its real path, so a symlinked `~/.tether` is
covered. The daemon, and anything you run outside an agent, can still write
all three. The sandbox needs bubblewrap (`bwrap`) and unprivileged user
namespaces. Where bubblewrap is missing or cannot build a namespace, every
launch Tether must sandbox is refused with 403 `forbidden` naming the fix,
rather than run unprotected. A launch whose work directory or workspace
lies inside a protected directory, the state directory included, is refused
with 403 `forbidden` too (not Codex's, which is not protected).

**Codex is NOT protected by Tether's write-protection.** Tether does not wrap
Codex in its sandbox, because Codex's own `workspace-write` sandbox is
bubblewrap too and cannot nest inside it where unprivileged user namespaces are
restricted (the Ubuntu default), which would stop Codex running any shell
command. Codex relies on its own sandbox, which makes the filesystem read-only
except Codex's working directories, `/tmp`, `$TMPDIR` and any configured
writable root, and a Codex turn's `touch <catalog>/x` is denied. But **Codex
spawns every MCP server it is given outside that sandbox**, with the
operator's uid and none of Codex's confinement, so a Codex agent can reach the
catalog through MCP tools:

- `torque_session_launch` over `torque mcp`, with a caller-chosen `workdir`:
  `workdir=<catalog>` boots a Codex whose own workspace-write root is the
  catalog (inferred from the real tool, not demonstrated end to end;
  CW-20261001-0464);
- `loom_export_bundle(dir=…)` over `loom mcp`, which wrote `index.md` and
  `log.md` into an arbitrary directory in a reproduction (CW-20261001-0465);
- the `nanite` (`dev_bash`, `dev_write`) and `cerberus` host shells, if a
  project grants them.

The default MCP allow-list (`torque`, `tesseract`) is therefore not safe for a
Codex agent either. **The structural fix is MCP upstreams that run daemon-side,
outside the agent's reach (CW-20261001-0230).** Until then `GET /health`
(`sandbox_protect.codex`), `mux doctor` (`sandbox-protect-codex`) and the
daemon's startup log say `codex: not protected (CW-20261001-0230)`, and
`mux doctor` warns. A Codex agent that is also a hostile worker can write the
catalog; treat Codex like the agents Tether did not protect before this change.

What does hold for Codex: the `mux mcp` Tether plants is started with
`--protect-path` for the catalog root, run directory and state directory, and
refuses to write under them (see below).

*History.* Earlier revisions of this change left Codex to its own sandbox under
an allowlist of flags, environment, injection, project config and MCP list,
re-checked before every turn, and called it guarded. Three adversarial reviews
found it could not be made sound while Codex spawns MCP servers outside its
sandbox, and that claim is withdrawn. The code stays in the tree, dormant behind
one switch (`codexProtectionMode`, `internal/app/protected_codex_mode.go`) until
CW-20261001-0230 makes it sound; [`docs/sandboxing.md`](docs/sandboxing.md#the-dormant-codex-guard)
lists what it would need.

Agents run under Tether's sandbox see a private PID namespace, lose
background processes when a per-turn CLI's turn ends, find `/home`, `~` and
`~/.tether` as separate mounts (a `rename` across them fails with `EXDEV`),
and cannot use `sudo`.

On macOS the protection is not applied yet. go-sandbox's seatbelt protection
has not been verified on a real Mac (CW-20261001-0138), so agents there can
still write all three directories. The daemon logs a warning at startup and
`mux doctor` reports one.

`mux doctor` asks the running daemon, whose environment decides protection,
and reports a `sandbox-protect` check and a `sandbox-protect-codex` check; `GET
/health` carries them as `sandbox_protect` (with `codex` and `codex_reason`).

An operator can turn the protection off by setting `TETHER_SANDBOX_PROTECT=0`
(or `false`) in muxd's environment. The daemon then logs a WARN line at
startup, and `mux doctor` reports a `sandbox-protect` warning. Agents can
write the catalog, run directory and state directory again, and ACP launches
are allowed.
Turning it off is a deliberate decision, never a silent default.

Where it applies, that stops an agent Tether wraps from writing those directories
itself. It does not yet cover:

- **Codex.** Not protected, as above: Codex's own sandbox keeps it out of the
  catalog, and Codex's MCP servers, which run outside that sandbox, do not
  (CW-20261001-0230).
- **Reads.** The directories are made read-only, not hidden: every agent, wrapped
  or not, can still read the catalog, including plaintext credentials in catalog
  YAML, and the state database, which holds every session, message and event
  (CW-20261001-0263).
- **The state database, for Codex.** The state directory is read-only for the
  agents Tether wraps. It is not for Codex, which Tether does not wrap: Codex's
  own sandbox keeps its shell out of the directory (unless `state_db` is in a
  directory that sandbox can write: `/tmp`, `$TMPDIR` or the work directory), and
  the planted `mux mcp` never opens the database, but Codex's MCP servers run outside that sandbox,
  so an upstream whose tool writes a caller-chosen path can still reach it
  (CW-20261001-0230). Any agent can still ask the daemon, over the socket, to
  write on its behalf.
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
  `scope=project`, which writes into the repo. The planted `mux mcp` is started with
  `--protect-path` for the catalog root, the run directory and the state directory,
  and refuses to write under them, with a typed `catalog_read_only` error telling the agent to ask the
  operator. That is a **policy of the planted server, for every runtime**, not an
  effect of a read-only mount, which exists only inside Tether's sandbox: Codex
  spawns MCP servers itself, outside it, and a Codex agent called
  `mux_agent_create scope=system` and wrote the catalog before the policy. The
  refusal holds against a symlink re-pointed while the call runs: on Linux the
  destination directory is opened once, judged by its identity and its ancestors',
  and the file is created relative to that open directory without following a
  symlink (an earlier check-then-write version lost the race: 3861 of 20000
  project-scope creates with `.tether` flipped between two symlinks, one into the
  catalog, wrote there). A symlinked directory that does not lead into a
  protected one (`.tether`, `agents/`) is followed as before; an agent file that is
  itself a symlink is not written through while protected paths are in force, and
  the tool answers with a typed `agent_file_is_symlink` error ("edit the link's target, or replace the link with a regular file"), not an internal error. The scope is not a boundary: a
  worker can start its own `mux mcp --scopes`, which a sandboxed agent finds
  read-only, and which a Codex agent runs under Codex's sandbox only.
- **The spec launch engine** (`TETHER_LAUNCH_ENGINE=spec`, off by default) takes
  argv, environment and injection from `~/.tether/launch-specs/`, which an agent can
  write. Do not enable it where an agent can write them.

The in-process API stub starts no agent process, so it has nothing to
protect.

**These protections are advisory against a hostile worker until caller identity
lands (CW-20260930-0253).** Any process that can reach `muxd.sock`, a worker
included, can call `POST /sessions` with `agent_inline` (extra flags and
environment per provider), `injection` and an MCP server list, and so shape the
launch it asks for. The pin on a catalog agent's sandbox profile (#85) closes
what that route can reach for the agents Tether wraps today; it does not make the
route itself authenticated, and it does nothing for Codex, which is not protected.

Until the socket is covered, and Codex's MCP servers run daemon-side, treat any agent Tether
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
- agents run as the operator's uid. On Linux, Claude and OpenCode agents (every
  agent Tether wraps) cannot write Tether's catalog, run directory or state directory
  themselves; Codex is not protected (CW-20261001-0230). Every agent, wrapped or
  not, can still read the catalog, including plaintext credentials in catalog
  YAML (CW-20261001-0263), and can call Tether's socket, which writes on its
  behalf. On macOS nothing stops them writing any of it yet (see "Agents run as
  your user")
- pre-1.0 contracts and migration guarantees

These are deployment constraints, not hidden roadmap promises. Operate within
them or place Tether behind controls that provide the missing boundary.
