# Catalog Launch And Boot Profiles

This is the operator reference for catalog-backed launches. Launch profiles
choose the project, agent, provider, and workspace strategy. Boot profiles add a
dynamic boot prompt on top of a launch.

For provider-specific setup examples, current smoke results, and boot prompt
generation workflow, see [`docs/launches/`](launches/README.md).

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

### Workspace roots and worktree lifecycle

Three roots are easy to confuse — they are distinct:

| Root | What it is | Mutated/removed by Tether? |
|---|---|---|
| `repo_root` | The source checkout a launch derives from. | Never. Tether never edits or deletes `repo_root`. |
| `work_root` | The editable/exec root the provider runs against. In `shared`/`hybrid` mode it aliases `repo_root`; in `worktree`/`isolated` mode it is a freshly materialized git worktree at `<base>/repo`. | In worktree mode only — see retention below. |
| `workspace_dir` | The per-session bookkeeping directory (`logs/`, `prompts/`, `state/plan.json`, …) under the workspace root. Distinct from `work_root`. | Removed by `mux workspaces prune`. |

`worktree_name` is a per-launch field. If set to a plain git ref (no spaces, no
Go-template `{{ }}` markers) it becomes the branch name for the materialized
worktree (`git worktree add -b <name>`). A template-style value such as
`tether/{{.ProjectID}}/{{.SessionID}}` is currently **reserved** — Tether has no
renderer for it yet, so such a value is ignored and the worktree is created
detached rather than feeding an unrendered string to git. An empty
`worktree_name` always yields a detached worktree.

If the target worktree path already exists, or git rejects the
`worktree add` because the path/branch is already registered, materialization
fails with an actionable error naming the `git worktree remove` / `git worktree
prune` remedy — it never clobbers an existing checkout.

**Retention / preservation policy:**

- **`boot-exec` worktrees** are removed on process exit (a `defer` calls
  `RemoveMaterializedWorkRoot`). A one-shot exec leaves nothing behind.
- **Failed session create** — if any step after worktree materialization fails,
  `createSessionFromPlan` removes the worktree it just created before returning
  the error, so a failed create never leaks a worktree or a stale registration.
- **Daemon-managed sessions** that reach a terminal state (`completed`,
  `failed`, `killed`) **keep their worktree and `workspace_dir`** — the work
  product and logs stay inspectable. Session teardown does *not* remove the
  worktree.
- **Explicit reclaim** is `mux workspaces prune`: it removes `workspace_dir`s
  for terminal/orphaned sessions older than `--older-than`, and for
  worktree/isolated-mode sessions it first runs `git worktree remove` against
  the source repo so the repo's worktree registry stays consistent — a bare
  directory delete would leave a stale worktree registration. `shared`/`hybrid`
  sessions have no worktree to deregister; only their `workspace_dir` is
  removed, never `repo_root`.

Removing a *terminated* session's worktree never affects a running session, and
attach (log replay) and resume (which creates a fresh session) are unaffected.

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

> **Persisted at rest — non-secret content only.** Injected `content`, and the
> file body read from `source`, are resolved into the launch plan and persisted
> verbatim as JSON in the `launch_plans` table. Never place API keys, tokens,
> or other secrets in `injection`. Secrets must flow through provider env
> `passthrough`/`whitelist` mode (which pulls from the live parent environment
> and is *not* persisted) or an external api-key-helper/keychain. The same rule
> applies to `overrides.env` — explicit env overrides are persisted too.

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
`mux boot-exec <profile>`. `mux boot-exec` and Tier-2 launch calls that provide
`BootProfileFile` regenerate boot-profile prompts after the worktree exists, so
`identity.work_root` can point at the materialized worktree. `mux boot` renders
the prompt client-side before creating the managed session.

> **`boot-exec` is Claude-TUI-only.** `mux boot-exec` execs directly into the
> native Claude PTY runtime; launch profiles whose provider is Codex or
> Opencode are rejected with a clear error. This is a boundary of the
> direct-exec convenience path, not a provider gap — Codex and Opencode are
> fully supported as managed sessions. For those providers use
> `mux boot <profile>` or `mux launch`, which create a daemon-managed session
> you attach to. See `docs/adr/0039-boot-exec-claude-only-scope.md`.

Useful commands:

```bash
mux list-boot-profiles
mux generate-boot tether-launcher.frontend.tui
mux boot tether-launcher.frontend.tui
mux boot-exec tether-launcher.frontend.tui
mux resolve --launch tether-launcher-claude-tui
```
