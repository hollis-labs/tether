# Catalog Launch And Boot Profiles

This is the operator reference for catalog-backed launches. Launch profiles
choose the project, agent, provider, and workspace strategy. Boot profiles add a
dynamic boot prompt on top of a launch.

## File Layout

```text
<catalog>/
  projects/<id>.yaml
  agents/<id>.yaml
  providers/<id>.yaml
  launches/<id>.yaml
  boot-profiles/<id>.yaml
  sandbox-profiles/<id>.yaml
  mcp-servers/<id>.yaml
```

## Launch Profile

```yaml
id: tether-launcher-claude-tui
project: tether-launcher
agent: frontend
provider: claude-pty
workspace:
  mode: worktree
  write_home: ~/.tether/workspaces/tether-launcher
  worktree_name: "tether/{{.ProjectID}}/{{.AgentID}}/{{.SessionID}}"
prompt:
  include_project_boot: true
  include_agent_boot: true
  include_knowledge_base: false
overrides:
  env: {KEY: value}
mcp:
  servers: [tesseract]
injection:
  native_files:
    - rel_path: .mux/context.md
      source: boot/context.md
      mode: 0644
    - rel_path: .mux/inline.json
      content: |
        {"source":"catalog"}
  boot_dir_overlay:
    - rel_path: extra.md
      content: |
        Additional provider boot-dir content.
```

Required fields are `id`, `project`, `agent`, and `provider`.

Workspace modes:

| Mode | Behavior |
|---|---|
| `worktree` | Create a per-session git worktree and run the provider against that `work_root`. |
| `isolated` | Alias for `worktree`. |
| `hybrid` | Run against `repo_root` while still using a session workspace for logs/prompts. |
| `shared` | Alias for the shared `repo_root` behavior. |

Unset `workspace.mode` resolves to the project `workspace.default_mode`; if both
are unset, the default is `worktree`.

Tether compiles launch profiles through `go-agent-launch` and stores shared
provenance (`plan_hash`, compiler version, provider/runtime/workspace, and
bootdir layout intent) in the persisted launch plan. Managed sessions and
`boot-exec` both use `go-agent-launch/providerplant` to render provider boot
files, native files, and overlays before handing the prepared launch to the
runtime. Tether still owns catalog compatibility, sandbox profiles, attach/
detach, and provider session ID persistence.

### Injection

Launch profiles can declare extra files that should be planted with the
prepared launch:

```yaml
injection:
  native_files:
    - rel_path: .mux/handoff.md
      source: handoffs/current.md
    - kind: skill
      id: local-helper
      content: |
        Use this helper when triaging local failures.
  boot_dir_overlay:
    - rel_path: CLAUDE.md
      source: boot/claude-overlay.md
```

`native_files` are passed to `go-agent-launch` as provider-aware native files.
For ordinary files, omit `kind` or set `kind: raw` and provide `rel_path`.
`kind: skill` requires `id`; the shared launch layer maps it to the provider's
native skill location where that provider supports one.

`boot_dir_overlay` writes directly into the prepared provider boot directory
after the provider's default files and native files are planted. Use it for
intentional provider boot-file overrides or extra boot-dir-only artifacts.

Each injected file must provide exactly one content source:

| Field | Meaning |
|---|---|
| `content` | Inline file body. |
| `source` | File path to read. Relative paths resolve from the catalog root; `~` and absolute paths are accepted. |
| `mode` | Optional file mode for `native_files`; defaults are applied by the shared launch layer when unset. |

Tether preserves launch-profile native files and appends compiled
`agent.skills` after them. This makes catalog/profile files the stable base and
provider-specific skill compilation an additive layer.

## Boot Profile

```yaml
id: tether-launcher.frontend.tui
display_name: "Tether Taxi - Frontend TUI"
launch: tether-launcher-claude-tui
mcp_servers: [tesseract, torque]
identity:
  profile_id: tether-launcher-frontend-tui
  role: frontend
  project: tether-launcher
  work_root: ~/dev/hollis-labs/apps/tether-launcher
slots:
  agent:
    type: role_summary
    path: ~/.nanite/roles/domain/frontend.md
```

`launch` is required when using `mux boot <profile>` or
`mux boot-exec <profile>`. When worktree mode is active, Tether regenerates
boot-profile prompts after the worktree exists so `identity.work_root` points at
the materialized worktree.

Useful commands:

```bash
mux list-boot-profiles
mux generate-boot tether-launcher.frontend.tui
mux boot tether-launcher.frontend.tui
mux boot-exec tether-launcher.frontend.tui
mux resolve --launch tether-launcher-claude-tui
```
