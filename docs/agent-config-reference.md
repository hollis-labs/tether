# Agent Config Reference (v005-08)

Tether's two-tier agent configuration model. See ADR 0033 for the design rationale.
For launch profile, boot profile, workspace, and slot fields, see
`docs/catalog-launch-profiles.md`.

Global environment identity, protocol and worker/hub module settings are
documented in [environment protocol and profiles](environment-protocol.md).

## Provider hosting

`catalog.defaults.launch_host` selects `direct` (the default) or opt-in `shim`
hosting for Claude streaming-stdio. `TETHER_LAUNCH_HOST` takes precedence and is
read on every launch; any non-empty value other than `shim` selects `direct`.
Unsupported runtimes and known pre-child placement or confinement failures keep
the direct launch path. Uncertain placement outcomes retain the child identity
and never create another provider. `tether doctor` reports this selection and
retained detached placements without contacting the host.

`catalog.defaults.shim_host` contains `journal_bytes` (256 MiB by default,
minimum 2 MiB), `systemd_user` (false by default), and `unit_prefix`
(`tether-shim-` by default). Production restart survival requires a planned
systemd-user activation. Detached hosting stays in the daemon's cgroup. See
[shim hosting](shim-host.md) for sandbox requirements, delivery windows and
operator-visible recovery limits.

## Discovery layers

Three layers, searched in order. Later layers override earlier ones on ID collision; missing layers are skipped silently.

| Layer | Root | Use |
|---|---|---|
| `system` | `<catalogPath>` (default `~/.tether/catalog/`) | Tether bundled defaults; managed via `tether agents create --scope system`. |
| `user` | `~/.tether/` | Personal customization. Default write target for `tether agents create`. |
| `project` | `./.tether/` (CWD-relative) | Repo-local overrides. Highest precedence. |

Each layer can contain three subdirectories:

```
<layer-root>/
  agents/        # *.yaml — Agent definitions
  boot-profiles/ # *.yaml — bootgen.Profile entries
  skills/        # *.md   — Skill files (frontmatter + body)
```

`tether agents list` shows the resolved view with a `LAYER` column.

## Agent schema

```yaml
id: my-agent                       # required, unique per layer
name: My Agent                     # required, human-readable
roles: [backend, refactor]         # optional, list of role tags
skills: [refactor-go, lint-fix]    # optional, references skill IDs (resolved via discovery)
context_files: []                  # optional, deprecated — prefer boot_fragments
boot_fragments: []                 # optional, paths inserted into the boot prompt
permissions:
  network: false                   # default false
  default_sandbox: workspace-only  # optional, references catalog sandbox-profiles/
system_prompt: |                   # optional v005-08
  You are a careful refactorer.
agent_prompt: |                    # optional v005-08
  Persona / who-am-I content.
provider_overrides:                # optional v005-08
  claude-code:
    env: {CLAUDE_FLAG: "1"}
    extra_args: ["--allow-foo"]
  codex-app-server:
    env: {CODEX_FLAG: "1"}
```

Empty / omitted fields take their zero values; existing pre-v005-08 catalog YAML loads unchanged.

### Worker identity: `urn` and `mcp_scopes`

Two optional agent fields apply to sessions placed on a worker (a launch with
`worker:` or `--worker`). Both are read from the **catalog** agent only; an
`agent_file`/`agent_inline` override cannot set them.

```yaml
urn: msg://agent/agent-mux/agt_454f2timi7   # stable registry URN this agent acts as
mcp_scopes: [message.write, registry.write, groups.write]
```

- `mcp_scopes` are the scopes of the session-bound credential the hub mints for
  the session (any of `session.write`, `message.write`, `registry.write`,
  `groups.write`, `catalog.write`, `ai.invoke`, `team.write`). Omitted means
  `message.write`, `registry.write`, `groups.write`, which matches the static
  scopes workers used before. Forum and channel read/write tools need no scope.
  An unknown scope fails catalog validation. Inherited through `extends`.
- `urn` is looked up in the registry, never minted from the catalog (the registry
  mints URNs on registration). The URN must be registered and a valid
  `msg://agent/<authority>/<id>`; two agents cannot declare the same URN. It is
  **not** inherited through `extends`, and team members never take it. At session
  creation the hub leases a runtime binding for the URN to the session, so the
  session's bearer resolves in `GET /auth/context` to `principal_kind=session`,
  its `session_id` and `agent_urn` = the stable URN. The binding is released and the
  credential revoked when the session ends, is stopped or is force-closed.
  If another live session holds the URN the launch fails with
  `stable agent identity is already bound to another live session` and names the
  holder; a lease left by an ended session is released automatically.

Both need a worker that advertises the `session-cred` feature (a `tether-worker`
built from this change). A session on an older worker keeps the static
`hub-mcp.json` entry and runs as the operator; if its agent declares `urn` the
launch is refused with a message to upgrade the worker, because it could never
act as that identity. See [forums.md](forums.md#agent-identity-on-workers).

## Turning an agent off: `enabled`

`enabled: false` in the agent file blocks new launches and chat sends for every
launch that uses the agent, and a running session is left alone. Absent means on;
it is not inherited through `extends`. An agent is loaded when the daemon starts,
so the change applies after a restart. Details, the API behaviour and the deploy
order are in [catalog-launch-profiles.md](catalog-launch-profiles.md#turning-an-agent-or-launch-off-enabled).

## Launch `worker`

A launch entry accepts an optional `worker: <label>`, the default worker for
sessions started from it by API-driven paths such as chat send. Omitted means
no default (those paths return `worker_not_configured`). See
[catalog-launch-profiles.md](catalog-launch-profiles.md#default-worker-worker).

## Boot profile schema (`bootgen.Profile`)

```yaml
id: my-agent.research.main
display_name: "My Agent — Research"
launch: my-agent-research-launch   # optional, catalog launch ID
identity: { ... }                  # Agent Identity Model fields (see internal/bootgen/profile.go)
slots: { ... }                     # boot prompt slot sources (static / role_summary / skill_index / cmd / http)
mcp_servers: [vanta, hadron]       # optional v005-08 — MCP allowlist for this profile
```

`mcp_servers` empty / omitted means the default allow-list, `torque` and `tesseract`. A non-empty list replaces it, so name `torque` and `tesseract` too if the agent needs them. The launch path pipes the list into `TETHER_MCP_SERVERS` and starts the spawned agent's `tether mcp --proxy` with `--confine`, so upstreams outside the list are not started and cannot be reached through `tether_tool_call`. See [mcp.md](mcp.md#agents-tether-launches-strict-config-and-an-allow-list).

Use `type: role_summary` for the `agent` slot when a full role markdown file should remain fetchable by path without being inlined into every boot prompt:

```yaml
slots:
  agent:
    type: role_summary
    path: ~/.nanite/roles/domain/backend/worker.md
```

## Resolution at launch

For Tier-1 (catalog-only) launches:

1. Launch profile resolves project + agent + provider from the catalog.
2. Launch-profile `injection.native_files` and `injection.boot_dir_overlay` are resolved from inline content or catalog-root-relative source files.
3. Skills referenced by `agent.skills` are loaded via discovery, compiled for the resolved provider, and appended to the native files list.
4. Provider-overrides (`agent.provider_overrides[providerID]`) apply env + extra args.
5. `system_prompt` and `agent_prompt` are appended to the boot prompt under `# System` / `# Agent` headings.

For Tier-2 (caller-provided) launches, see `docs/caller-launched-sessions.md`.

## `tether agents` CLI

```
tether agents list                                   # layered listing
tether agents create <id> --scope user|project|system \
                       --name "..." \
                       --system-prompt "..." \
                       --agent-prompt "..."       # writes a new agent YAML
tether agents edit <id>                              # opens in $EDITOR
tether agents show <id>                              # prints layer-annotated YAML
```

See ADR 0033 for full rationale.
