# What an agent Tether launches can still reach

Tether is a single-user, same-host control plane. An agent it launches runs
under the operator's own uid, so every layer below narrows what the agent can
do by default; none of them is a boundary against a hostile agent. This page
is the one place that says **which layer covers which resource, and what is
still open**, so that nobody has to reconstruct it from five changelog entries.
[SECURITY.md](../SECURITY.md) states the deployment boundary; this page is the
inventory.

> **Draft.** This describes the state once the open layers below have landed.
> A row that depends on an unmerged change says so. Update it when a
> follow-up lands: move the item out of "still open" and into "protected how".

## The layers

| Change | What it does | State |
|---|---|---|
| CW-20261001-0130 (#84) | A launch of an agent that names a sandbox profile the catalog does not define is refused (404), instead of running with no sandbox | merged |
| CW-20261001-0145 (#85) | A session-create override (`agent_file`, `agent_inline`) may not change a catalog agent's pinned sandbox profile (403); it may only tighten an agent that has none | merged |
| CW-20261001-0142 (#87) | On Linux, the catalog root and the daemon's run directory are read-only for every launched agent, under `bwrap`; ACP launches are refused while it is on | open, being revised: codex runtimes are to be exempt from the outer `bwrap`, because codex's own sandbox is `bwrap` and nested `bwrap` is refused where AppArmor restricts unprivileged user namespaces, so they rely on codex's own sandbox |
| CW-20261001-0227 (#88) | A launched Claude agent loads only the MCP servers Tether plants (`--strict-mcp-config`), and every launched agent's `mux` proxy is confined (`--confine`) to an allow-list of upstreams | open |
| CW-20261001-0173 (#89) | The `mux mcp` planted in each agent is `--daemon-only`: it never opens the state database, reads Tether's state through the daemon, and the daemon records each tool call | open |
| CW-20261001-0173, second PR | The state directory joins the read-only set. Lands after #89 and #87, never before: until the planted server is daemon-only, a read-only state directory breaks every agent's mux tools | not yet opened |

## The matrix

"Protected" means **write-protected** for a filesystem resource. Nothing in the
table hides a file from an agent unless it says so; reads are their own row.

| Resource | Protected how | Still open | Follow-up |
|---|---|---|---|
| **Catalog root** (launch, agent, provider and MCP-server definitions, and the plaintext credentials they hold) | Read-only for the agent, by real path, on Linux (#87). The daemon and the operator can still write it | Reading it. macOS: not applied. `TETHER_SANDBOX_PROTECT=0` turns it off (the daemon logs a WARN, `mux doctor` warns). Codex runtimes rely on codex's own sandbox, not Tether's (being changed in #87) | CW-20261001-0263 (read denial), CW-20261001-0138 (macOS) |
| **Run directory** (`muxd.pid`, `muxd.sock`) | Read-only for the agent (#87), when it lies inside Tether's root | Connecting to the socket: see the next row | CW-20260930-0253 |
| **The daemon's HTTP API** (`muxd.sock`) | Nothing stops `connect(2)`. The override rule (#85) stops an agent from loosening its own sandbox through a child session | The socket is the full, unauthenticated API, including writes the daemon makes on the caller's behalf. The MCP token and scopes are checked inside the agent's own `mux mcp`, so an agent that calls the socket directly bypasses them. The #85 rule is interim and is removed when identity lands | CW-20260930-0253 (verified caller identity), CW-20260918-0037 (the audit it came from) |
| **State database** (`defaults.state_db`: every session, message and event) | The planted `mux mcp` never opens it (#89). The directory is read-only for the agent (second PR), so the agent cannot write the database or replace it or its WAL files. A `mux` command an agent runs by hand that opens the database fails inside the sandbox | Reading it, including other agents' sessions and messages. Writes the daemon makes for the agent through the socket | CW-20261001-0263 (read denial), CW-20260930-0253 |
| **The agent's own sandbox profile** | An undefined profile refuses the launch (#84). An override cannot change a pinned profile (#85). An agent with no profile runs under a minimal profile whose only effect is the protection above | Everything else about an agent without a pinned profile: Tether leaves it unconfined beyond the protected directories. The #85 rule goes away with identity | CW-20260930-0253 |
| **MCP servers the agent can call** | A Claude agent loads only the servers Tether plants, so none of the operator's `~/.claude.json` servers, project `.mcp.json` files or claude.ai connectors (#88; `TETHER_CLAUDE_STRICT_MCP=0` turns it off). The planted proxy reaches only its allow-list, by default `torque` and `tesseract` and never `cerberus`; an upstream outside it is never started or resolved and is not reachable through `mux_call` (#88) | An agent that goes looking can create a child session through the API with a wider list, or run an upstream's binary itself. Strict loading is Claude-only: #88 does not cover codex or opencode, and what they load beyond the planted server is not verified | CW-20260930-0253, CW-20261001-0230 |
| **Upstream MCP credentials** (tokens for `tesseract`, `hadrond`, …) | A non-allow-listed upstream's secrets are never read into the agent's proxy (#88) | An allow-listed upstream runs as a child of the agent's own proxy, with its token on argv, readable by any same-uid process through `/proc/<pid>/cmdline`. The catalog entries that hold them are readable | CW-20261001-0230 (run upstreams daemon-side), CW-20261001-0228 (decision: rotate and move), CW-20261001-0229 (take a credential from a 0600 file), CW-20261001-0257 and CW-20261001-0258 (token from environment or file) |
| **Tool-call records** (`proxy_events`, the event log) | The planted server cannot write them. It asks the daemon, which checks the record's shape, caps its size and stamps the time itself (#89) | The tool name, server and outcome are the agent process's word until the daemon knows who is calling. Calls to native mux tools are recorded with an empty `session_id` | CW-20260930-0253, CW-20260912-0074 |
| **User and project definition layers** (`~/.tether/agents`, `skills`, `boot-profiles`, a project's `.tether/`) | Nothing. They sit outside the catalog root | Writable. An agent can plant a definition that a later launch resolves | CW-20261001-0192 |
| **ACP agents** (Copilot, Pi) | Launches are refused while protection is on (#87) | They cannot be launched while protection is on, until Tether can apply a protect-only sandbox to an ACP agent | CW-20261001-0162 |
| **`mux boot` and `mux boot-exec`** | None. They run the operator's own Claude, in the operator's terminal, with the operator's own MCP servers. The planted `mux mcp` there is an ordinary one that opens the database | Outside the daemon's launch path by design | none |
| **Writes delegated to same-uid services** | None | An agent can ask a service running as the same user to write for it, such as `systemd-run --user` or the daemon over its socket. Inherent to running as the operator's uid | CW-20260930-0253, CW-20261001-0263 |
| **Loopback services of sibling apps** | None from Tether | Services listening on loopback, such as Nanite's `/api/tools/call`, are reachable from a sandboxed agent | CW-20261001-0212 (Nanite) |
| **Everything else the operator can read or write** (the rest of the home directory, other repositories, SSH keys) | None. The sandbox is the host filesystem with the protected directories read-only, not an isolation boundary | Reading and writing it | CW-20261001-0263 covers the control plane only; wider isolation is out of scope here |

## Reading the table

- **Write protection is not hiding.** `bwrap` binds the host filesystem and
  makes the protected directories read-only. An agent can still read the
  catalog, with the credentials in it, and the state database. go-sandbox's
  `FS.Protect` is write-only, and its `FS.Deny` is refused on Linux in the
  host-filesystem mode Tether uses, so closing this is its own piece of work
  (CW-20261001-0263).
- **Two switches turn layers off, and neither is silent.**
  `TETHER_SANDBOX_PROTECT=0` and `TETHER_CLAUDE_STRICT_MCP=0` each log a WARN
  at daemon start and make `mux doctor` warn.
- **Identity is the common follow-up.** Most "still open" cells name
  CW-20260930-0253. Until the daemon can tell an agent from the operator,
  anything an agent can ask the daemon to do, it can do.
