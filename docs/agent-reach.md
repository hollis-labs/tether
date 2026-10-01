# What an agent Tether launches can still reach

Tether is a single-user, same-host control plane. An agent it launches runs
under the operator's own uid, so every layer below narrows what the agent can
do by default; none of them is a boundary against a hostile agent. This page
is the one place that says **which layer covers which resource, and what is
still open**, so that nobody has to reconstruct it from a dozen changelog
entries. [SECURITY.md](../SECURITY.md) states the deployment boundary; this
page is the inventory.

> **Three things to read first.**
>
> 1. **Today an agent can read the control plane.** The catalog, including
>    the plaintext credentials in it, the state database, which holds every
>    session and message, and the MCP tokens are all readable by an agent.
>    Every protection below that protects a directory protects it from being
>    *written*; none of them hides it. Tracked as CW-20261001-0263.
> 2. **Codex is not protected by Tether's write-protection** (as of #87).
>    Claude and OpenCode agents run with the catalog root and the run
>    directory read-only, and a write attempt fails with "Read-only file
>    system". Codex runs under its own `workspace-write` sandbox instead,
>    which keeps its *shell* out of the catalog, but **Codex spawns every MCP
>    server it is given outside that sandbox**, at the operator's uid. A Codex
>    agent can therefore reach the catalog through MCP tools. `GET /health`,
>    `tether doctor` and the daemon log say `codex: not protected
>    (CW-20261001-0230)`. See the Codex row.
> 3. **All of it is advisory against a hostile worker until the daemon can
>    tell an agent caller from the operator** (CW-20260930-0253). Any process
>    that can reach `tetherd.sock`, a worker included, can call `POST /sessions`
>    with `agent_inline` (extra flags and environment per provider),
>    `injection` and an MCP server list, and so shape the launch it asks for.
>    Scoping the MCP tool is not a boundary, because the daemon API accepts the
>    same fields from anyone on the socket. The sandbox-profile rule (#85), the
>    MCP allow-list (#88) and the catalog policy guard are all advisory against
>    such a worker until then.
>
> The protection is **Linux-only** (macOS is CW-20261001-0138), and an
> operator can turn it off with `TETHER_SANDBOX_PROTECT=0` (or `false`) in
> tetherd's environment.

> **Status.** #84, #85, #87, #88, #89 and #90 are merged. The one layer still
> open is the state-directory PR (#93, a draft), and the rows that depend on it
> say so. Update this page when a follow-up lands: move the item out of "still
> open" and into "protected how".

## The layers

| Change | What it does | State |
|---|---|---|
| CW-20261001-0130 (#84) | A launch of an agent that names a sandbox profile the catalog does not define is refused (404), instead of running with no sandbox | merged |
| CW-20261001-0145 (#85) | A session-create override (`agent_file`, `agent_inline`) may not change a catalog agent's pinned sandbox profile (403); it may only tighten an agent that has none | merged |
| CW-20261001-0173 (#89) | The `tether mcp` planted in each agent is `--daemon-only`: it never opens the state database, reads Tether's state through the daemon, and the daemon records each tool call | merged |
| CW-20261001-0227 (#88) | A launched Claude agent loads only the MCP servers Tether plants (`--strict-mcp-config`), and every launched agent's `tether` proxy is confined (`--confine`) to an allow-list of upstreams, in the daemon-only proxy as well as the in-process one | merged |
| CW-20261001-0229 (#90) | An MCP-server catalog entry can take a credential from a 0600 file (`file://`) instead of carrying it in the YAML or on a command line | merged |
| CW-20261001-0142 (#87) | On Linux, the catalog root and the daemon's run directory are read-only for every agent Tether wraps, under `bwrap`; ACP launches are refused while it is on. **Codex is not wrapped and not protected**, and is flagged as such. The planted `tether mcp` refuses `tether_agent_create` and `tether_agent_edit` writes under the catalog and run directory for every runtime, Codex included | merged |
| CW-20261001-0173, second PR | The state directory joins the read-only set for the agents Tether wraps | open, draft (#93), until it lands |

## The matrix

"Protected" means **write-protected** for a filesystem resource. Nothing in the
table hides a file from an agent unless it says so; reads are their own row.

| Resource | Protected how | Still open | Follow-up |
|---|---|---|---|
| **Reading the control plane** (the catalog's plaintext credentials, the state database with every session and message, the MCP tokens in each `.mcp.json` and in the operator's client configs) | **Nothing.** Write protection does not cover reads | **Today an agent can read all of it.** The credentials and tokens are readable by the agent, and the state database holds every other agent's sessions and messages. This is a gap in every row below | CW-20261001-0263 |
| **Catalog root** (launch, agent, provider and MCP-server definitions, and the plaintext credentials they hold) | For Claude, OpenCode and every other agent Tether wraps, on Linux: a read-only bind of the real path (#87); a write attempt fails with "Read-only file system". For **every** runtime, Codex included: the planted `tether mcp` is started with `--protect-path` and refuses `tether_agent_create` and `tether_agent_edit` writes under the catalog and run directory with a typed `catalog_read_only` error. That is a policy of the planted server, not a mount, and it holds against a symlink re-pointed while the call runs (on Linux the destination directory is opened once and judged by its device and inode). It is **best-effort for Codex**: if the protected directories cannot be named, a Codex launch still goes ahead with no `--protect-path` and a logged WARN, so the planted server does not refuse for that launch. It is also absent when protection is off (the kill switch) and on macOS. Planted workers keep `catalog.write`, since it also gates `scope=project`, which writes into the repository and is not protected | **Reading it** (first row). **Codex is not covered** (Codex row). macOS: not applied. `TETHER_SANDBOX_PROTECT=0` turns it off (the daemon logs a WARN, `tether doctor` warns) | CW-20261001-0263 (read denial), CW-20261001-0466 (Codex), CW-20261001-0138 (macOS) |
| **Run directory** (`tetherd.pid`, `tetherd.sock`) | Read-only for the wrapped agents (#87), when it lies in the catalog's parent or in `~/.tether`; a pid file in a shared directory such as `/tmp` is skipped | Connecting to the socket: see the next row. Codex: as above | CW-20260930-0253 |
| **The daemon's HTTP API** (`tetherd.sock`) | Nothing stops `connect(2)`, and the socket is reachable from inside every agent's sandbox by design, since the planted proxy needs it | The socket is the full, unauthenticated API, including writes the daemon makes on the caller's behalf. The MCP token and scopes are checked inside the agent's own `tether mcp`, so an agent that calls the socket directly bypasses them | CW-20260930-0253 (verified caller identity), CW-20260918-0037 (the audit it came from) |
| **Caller-supplied launch overrides** (`agent_inline`, `provider_overrides.extra_args`, `injection`, an MCP list, from a planted worker, over the MCP tool or straight at `POST /sessions`) | #85 stops an override from changing a pinned sandbox profile. #88's allow-list and #87's guards limit what a launch is handed by default | **All advisory.** The daemon accepts the same fields from anyone on the socket, so a worker can ask for a launch with wider flags, environment, injection or MCP list. #85's rule closes only the sandbox-profile case, and #88 already notes an agent can widen its MCP list this way | CW-20260930-0253 |
| **State database** (`defaults.state_db`: every session, message and event) | The planted `tether mcp` never opens it (#89). For the wrapped agents the directory is read-only (second PR), so the agent cannot write the database or replace it or its WAL files; a `tether` command such an agent runs by hand that opens the database fails inside the sandbox | **Reading it today**, including every other agent's sessions and messages (first row). **Codex:** its shell is kept out by Codex's own sandbox, and its planted `tether mcp` holds no handle on the database, but an MCP upstream whose tool writes a caller-chosen path can still reach it. Writes the daemon makes for the agent through the socket | CW-20261001-0263 (read denial), CW-20261001-0230, CW-20260930-0253 |
| **The agent's own sandbox profile** | An undefined profile refuses the launch (#84). An override cannot change a pinned profile (#85). An agent with no profile runs under a minimal profile whose only effect is the protection above | Everything else about an agent without a pinned profile: Tether leaves it unconfined beyond the protected directories. The #85 rule goes away with identity | CW-20260930-0253 |
| **Codex runtimes** (`exec`, `app-server`) | **Not protected by Tether's write-protection, as of #87.** Tether does not wrap Codex: its own `workspace-write` sandbox is `bwrap` too, and `bwrap` cannot nest where AppArmor restricts unprivileged user namespaces, so wrapping it would stop it running any shell command. Codex runs under that sandbox as it does on main, which still denies its shell the catalog, the run directory and the state directory ("Read-only file system", seen live with real codex in `exec` and `app-server`). The one thing that holds for Codex is the planted `tether` server's `catalog_read_only` refusal, above | **Codex spawns every MCP server it is given outside its sandbox**, at the operator's uid, so a Codex agent can reach the catalog through MCP tools: `torque_session_launch` with a chosen `workdir` (CW-20261001-0464, reported: inferred from the real tool, not demonstrated end to end), `loom_export_bundle(dir=…)` writing into an arbitrary directory (CW-20261001-0465, reproduced), the `nanite` and `cerberus` host shells if a project grants them, and `tangent.runner_launch`, which runs any host command the caller names. The default allow-list (`torque`, `tesseract`) is not safe for Codex either. `GET /health` (`sandbox_protect.codex`), `tether doctor` (`sandbox-protect-codex`, a warning) and the startup log say `codex: not protected (CW-20261001-0230)` | CW-20261001-0466 (the Codex finding, p1), CW-20261001-0230 (the structural fix: upstreams that run daemon-side), CW-20261001-0464, CW-20261001-0465 |
| **MCP servers the agent can call** | A Claude agent loads only the servers Tether plants, so none of the operator's `~/.claude.json` servers, project `.mcp.json` files or claude.ai connectors (#88; `TETHER_CLAUDE_STRICT_MCP=0` turns it off). The planted proxy reaches only its allow-list, by default `torque` and `tesseract` and never `cerberus`; an upstream outside it is never started or resolved and is not reachable through `tether_tool_call` (#88) | An agent that goes looking can create a child session through the API with a wider list, or run an upstream's binary itself. Strict loading is Claude-only: #88 does not cover codex or opencode, and what they load beyond the planted server is not verified. **For a Codex agent, the planted proxy and every upstream it starts run outside any sandbox:** `--confine` still limits which upstreams start, but each started upstream has its full tool surface at the operator's uid | CW-20260930-0253, CW-20261001-0230 |
| **Upstream MCP credentials** (tokens for `tesseract`, `hadrond`, …) | A non-allow-listed upstream's secrets are never read into the agent's proxy (#88). A catalog entry can now take a credential from a 0600 file, and a reference in `env:` keeps it off the command line (#90) | An allow-listed upstream runs as a child of the agent's own proxy. Its token is on argv unless it comes from `env:` or a file, and `tesseract mcp` and `hadrond mcp` currently take their token only as `--token`, so it is readable by any same-uid process through `/proc/<pid>/cmdline`. The catalog entries that hold them are readable. Whether the operator's entries move to the new reference is a decision, not yet taken | CW-20261001-0230 (run upstreams daemon-side), CW-20261001-0228 (decision: rotate and move), CW-20261001-0257 and CW-20261001-0258 (token from environment or file) |
| **Tool-call records** (`proxy_events`, the `tool_call_*` events) | The planted server cannot write them. It asks the daemon, which checks the record's shape and size, stamps the time itself, and refuses a published record for a session that does not exist (#89) | The tool name, server, outcome and session are the agent process's word: the daemon cannot tell which session is really calling, so a record for another existing session is accepted. Calls to native tether tools are recorded with an empty `session_id`. `POST /broker/envelopes` is another route that writes events with caller-supplied fields | CW-20260930-0253, CW-20260912-0074 |
| **User and project definition layers** (`~/.tether/agents`, `skills`, `boot-profiles`, a project's `.tether/`) | Nothing. They sit outside the catalog root | Writable. An agent can plant a definition that a later launch resolves | CW-20261001-0192 |
| **ACP agents** (Copilot, Pi) | Launches are refused while protection is on (#87) | They cannot be launched while protection is on, until Tether can apply a protect-only sandbox to an ACP agent | CW-20261001-0162 |
| **`tether boot` and `tether boot-exec`** | None. They run the operator's own Claude, in the operator's terminal, with the operator's own MCP servers. The planted `tether mcp` there is an ordinary one that opens the database | Outside the daemon's launch path by design | none |
| **Writes delegated to same-uid services** | None | An agent can ask a service running as the same user to write for it, such as `systemd-run --user` or the daemon over its socket. Inherent to running as the operator's uid | CW-20260930-0253, CW-20261001-0263 |
| **Loopback services of sibling apps** | None from Tether | Services listening on loopback, such as Nanite's `/api/tools/call`, are reachable from a sandboxed agent | CW-20261001-0212 (Nanite) |
| **Everything else the operator can read or write** (the rest of the home directory, other repositories, SSH keys) | None. The sandbox is the host filesystem with the protected directories read-only, not an isolation boundary | Reading and writing it | CW-20261001-0263 covers the control plane only; wider isolation is out of scope here |

## Reading the table

- **Write protection is not hiding.** `bwrap` binds the host filesystem and
  makes the protected directories read-only. An agent can still read the
  catalog, with the credentials in it, and the state database, with every
  agent's sessions and messages, and the MCP tokens (CW-20261001-0263).
  go-sandbox's `FS.Protect` is write-only, and its `FS.Deny` is refused on
  Linux in the host-filesystem mode Tether uses.
- **Codex is the exception to "wrapped".** Where a row says "the agents
  Tether wraps", it means Claude, OpenCode and the other runtimes, and not
  Codex. Where the row is about the planted `tether` server's own policy, it
  holds for every runtime.
- **Two switches turn layers off, and neither is silent.**
  `TETHER_SANDBOX_PROTECT=0` and `TETHER_CLAUDE_STRICT_MCP=0` each log a WARN
  at daemon start and make `tether doctor` warn.
- **Identity is the common follow-up.** Most "still open" cells name
  CW-20260930-0253. Until the daemon can tell an agent from the operator,
  anything an agent can ask the daemon to do, it can do.
- **The structural fix for MCP is CW-20261001-0230.** The planted proxy and
  the upstreams it starts run inside the agent's process tree, which is why
  credentials sit on argv and why a Codex agent's upstreams are unsandboxed.
  Running upstreams in the daemon removes both.
