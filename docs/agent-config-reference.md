# Agent Config Reference (v005-08)

Mux's two-tier agent configuration model. See ADR 0033 for the design rationale.

## Discovery layers

Three layers, searched in order. Later layers override earlier ones on ID collision; missing layers are skipped silently.

| Layer | Root | Use |
|---|---|---|
| `system` | `<catalogPath>` (default `~/.agent-mux/catalog/`) | Mux bundled defaults; managed via `mux agents create --scope system`. |
| `user` | `~/.agent-mux/` | Personal customization. Default write target for `mux agents create`. |
| `project` | `./.agent-mux/` (CWD-relative) | Repo-local overrides. Highest precedence. |

Each layer can contain three subdirectories:

```
<layer-root>/
  agents/        # *.yaml — Agent definitions
  boot-profiles/ # *.yaml — bootgen.Profile entries
  skills/        # *.md   — Skill files (frontmatter + body)
```

`mux agents list` shows the resolved view with a `LAYER` column.

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

## Boot profile schema (`bootgen.Profile`)

```yaml
id: my-agent.research.main
display_name: "My Agent — Research"
launch: my-agent-research-launch   # optional, catalog launch ID
identity: { ... }                  # Agent Identity Model fields (see internal/bootgen/profile.go)
slots: { ... }                     # boot prompt slot sources (static / role_summary / skill_index / cmd / http)
mcp_servers: [vanta, hadron]       # optional v005-08 — MCP allowlist for this profile
```

`mcp_servers` empty / omitted means the proxy default (all servers). The launch path pipes the list into `MUX_MCP_SERVERS` so the spawned agent's `mux mcp --proxy` sees it.

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
2. Skills referenced by `agent.skills` are loaded via discovery, compiled for the resolved provider, and appended to the boot prompt.
3. Provider-overrides (`agent.provider_overrides[providerID]`) apply env + extra args.
4. `system_prompt` and `agent_prompt` are appended to the boot prompt under `# System` / `# Agent` headings.

For Tier-2 (caller-provided) launches, see `docs/caller-launched-sessions.md`.

## `mux agents` CLI

```
mux agents list                                   # layered listing
mux agents create <id> --scope user|project|system \
                       --name "..." \
                       --system-prompt "..." \
                       --agent-prompt "..."       # writes a new agent YAML
mux agents edit <id>                              # opens in $EDITOR
mux agents show <id>                              # prints layer-annotated YAML
```

See ADR 0033 for full rationale.
