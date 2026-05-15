# Shared Agent Launch Reference

This is the handoff guide for apps adopting the shared launch stack used by
Tether. Torque and Nanite should follow this shape unless they have a specific
workflow reason to differ.

## Package Split

| Package | Responsibility |
|---|---|
| `go-agent-launch` | Compile launch plans, prepare argv/env/workdir, and plant provider boot dirs/native files/overlays. |
| `go-agent-context` | Assemble higher-level context payloads before a launch plan is built. |
| `go-agent-sessions` | Own long-lived process lifecycle, attach/detach, events, and session state. |
| Tether | Reference catalog install: YAML catalog, launch profiles, boot profiles, MCP/session APIs, and wiring into the shared packages. |

Apps should build their own business workflow around these packages, but should
avoid reimplementing boot-dir planting or provider/runtime argv construction.

## Launch Flow

1. Resolve the app's project/agent/provider/workspace inputs.
2. Build an `agentlaunch.LaunchPlan`.
3. Compile with `launcher.Compile`.
4. Prepare with `launcher.Prepare`.
5. Plant with `providerplant.Plant`.
6. Start directly with the prepared argv/env/workdir or convert to
   `go-agent-sessions` start options with `sessionshim.ToSessionLaunch`.

Tether's managed sessions do the same flow, then overlay Tether-owned session
fields such as logs, sandbox, attach policy, and provider session ID capture.
`boot-exec` uses the same compile/prepare/plant flow, then execs into the
prepared provider command so the user lands in the native TUI.

## Tether Launch Profile Injection

Tether launch profiles expose the shared injection surface as catalog YAML:

```yaml
injection:
  native_files:
    - rel_path: .mux/handoff.md
      source: handoffs/current.md
    - rel_path: .mux/session.json
      content: |
        {"source":"catalog"}
    - kind: skill
      id: local-helper
      content: |
        Use this helper when triaging local failures.
  boot_dir_overlay:
    - rel_path: extra.md
      content: |
        Additional provider boot-dir content.
```

Rules:

- `native_files` maps to `agentlaunch.InjectionSpec.NativeFiles`.
- `boot_dir_overlay` maps to `agentlaunch.InjectionSpec.BootDirOverlay`.
- `source` paths are resolved from the catalog root unless absolute or `~`
  prefixed.
- Each entry may set exactly one of `content` or `source`.
- `kind` defaults to `raw`; `kind: skill` requires `id`.
- Tether preserves catalog `native_files` and appends compiled `agent.skills`
  afterward.

## Skills

Tether treats skills as files planted at boot, not as boot-prompt text.
Provider-specific skill support is separate from the generic injection feature:

| Provider | Current Tether skill output |
|---|---|
| Claude | `.claude/skills/<id>.md` via native skill files. |
| Codex | Aggregated `AGENTS.md` raw native file. |
| Opencode | Not implemented yet; launch still works without compiled skills. |

Torque and Nanite can either reuse Tether's skill compiler shape or construct
`agentlaunch.NativeFile` values directly.

## Workspace Model

The preferred default is isolated worktrees:

- `repo_root`: canonical checkout; read-only by policy unless explicitly needed.
- `work_root`: per-launch materialized worktree; normal read/write/exec work.
- `workspace_dir`: per-session state root for boot dirs, logs, transient files,
  and metadata.

Tether materializes `work_root` before preparing the shared launch plan, so the
provider receives the isolated worktree as its execution root.

## Adoption Checklist For Torque And Nanite

1. Use `go-agent-launch` for compile/prepare/plant.
2. Stop hand-planting provider boot dirs in app code.
3. Represent app-specific context as `NativeFiles`, `BootDirOverlay`, or the
   boot prompt payload before compiling the launch plan.
4. Keep provider/runtime selection as data: provider plus runtime kind.
5. Use worktree isolation by default where the app launches write-capable agents.
6. Add live smoke tests for Claude, Codex, and Opencode paths the app exposes.
7. Keep app-specific orchestration outside the shared package; only common launch
   mechanics should move into shared libs.

