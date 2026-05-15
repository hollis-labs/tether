# Shared Launch Adoption Guide

This guide captures what we learned while making Tether the reference
implementation for shared agent launch. Torque and Nanite should use this as a
starting point, then adapt to their own workflow boundaries.

## Package Versions

Use tagged package releases, not local replaces:

- `github.com/hollis-labs/go-agent-launch v0.1.0`
- `github.com/hollis-labs/go-agent-context v0.1.0`
- `github.com/hollis-labs/go-agent-sessions v0.9.4`
- `github.com/hollis-labs/go-providers v0.17.1`

Tether may still have local `replace` lines during active development. Before
release or cross-app handoff, remove them and verify the same tests pass against
the tagged modules.

## Reference Files In Tether

Catalog/schema:

- `internal/config/model.go`
  - `Launch.Injection`
  - `LaunchInjection`
  - `InjectedFile`
- `internal/config/validate.go`
  - validates raw vs skill files, required paths/ids, and content/source
    exclusivity.
- `internal/launch/resolver.go`
  - resolves catalog-root-relative `source` files.
  - writes `NativeFiles` and `BootDirOverlay` onto `launch.Plan`.
- `internal/launch/plan.go`
  - persisted plan fields: `WorkRoot`, `WorkspaceMode`, `WorktreeBase`,
    `WorktreeName`, `NativeFiles`, `BootDirOverlay`, `Shared`.

Shared launch preparation:

- `internal/app/shared_launch.go`
  - maps Tether `launch.Plan` to `agentlaunch.LaunchPlan`.
  - passes `Injection.NativeFiles` and `Injection.BootDirOverlay`.
  - calls `providerplant.Plant`.
- `internal/app/session_lifecycle.go`
  - materializes worktree before persisting launch plan.
  - prepares shared launch before `go-agent-sessions` start.
  - converts prepared launch through `sessionshim.ToSessionLaunch`.
  - overlays app-owned fields such as logs, sandbox, attach, and provider
    session ID persistence.
- `internal/bootexec/claude.go`
  - direct native TUI path using the same compile/prepare/plant flow.

Skills and Agent Ops:

- `internal/app/agent_ops.go`
  - compiled skills become native files, not boot prompt text.
  - catalog native files are preserved, then compiled skills are appended.
- `internal/skills/skills.go`
  - Claude skills compile to `.claude/skills/<id>.md`.
  - Codex skills compile to `AGENTS.md`.
  - Opencode skill compilation is separate future provider work.

Workspace isolation:

- `internal/workspace/workroot.go`
  - `repo_root`: canonical checkout.
  - `work_root`: writable/exec per-launch worktree.
  - `workspace_dir`: session state, boot dirs, logs, metadata.

Docs:

- `docs/catalog-launch-profiles.md`
- `docs/shared-agent-launch-reference.md`
- `docs/skill-format.md`

## Recommended Adoption Shape

1. Resolve app-specific task/profile/context inputs.
2. Build an `agentlaunch.LaunchPlan`.
3. Compile with `launcher.Compile`.
4. Prepare with `launcher.Prepare`.
5. Plant with `providerplant.Plant`.
6. For managed sessions, convert with `sessionshim.ToSessionLaunch`.
7. Overlay only app-owned runtime fields after conversion.

App-owned fields usually include logs, persistence IDs, sandbox/permissions,
attach policy, task-scoped authorization, and app-specific event routing.

## Injection Model

Tether exposes this catalog shape:

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

Use `native_files` for provider-aware planted files. Use `boot_dir_overlay` only
for intentional provider boot-dir overrides or extra boot-dir-only artifacts.

Callers can also supply the same injection shape per-launch *outside* catalog
YAML via the Tier-2 `injection` field (a JSON-encoded `config.LaunchInjection`)
on the API/MCP/CLI surfaces. Caller native files are appended after catalog
native files (and before compiled `agent.skills`); caller `boot_dir_overlay`
entries merge into the catalog overlay map and the caller value wins on a
duplicate `rel_path`. Relative `source` paths in caller injection resolve from
the catalog/config root, not the process CWD.

> ⚠️ **Downstream adopters (Torque, Nanite): injection content is persisted at
> rest.** Both catalog `injection` and the Tier-2 caller `injection` field
> resolve into `launch.Plan`, which Tether persists verbatim as JSON in the
> `launch_plans` table. Any `content` value — and the file body read from a
> `source` path — is therefore stored unencrypted at rest. The same is true of
> explicit `overrides.env` entries. **Never route API keys, tokens, or other
> secrets through `injection.content`, `injection.source`, or `overrides.env`.**
> Secrets must flow through provider env `passthrough`/`whitelist` mode — which
> pulls from the live parent environment at launch time and is *not*
> materialized into the persisted plan — or through an external
> api-key-helper/keychain. Tether deliberately does not provide a separate
> non-persisted runtime-only injection layer today; if your app needs one,
> raise it for alignment rather than smuggling secrets through the persisted
> path.

## Sharp Edges

- Do not blindly copy Tether service code. Copy the boundary shape, not the
  app-specific session semantics.
- Avoid double planting. If `providerplant.Plant` is used before session start,
  set/ensure the session layer does not auto-plant the boot dir again.
- Avoid double args. Tether removes catalog base args from prepared argv before
  assigning session `ExtraArgs`.
- `source` path handling matters. Relative injection paths should resolve from
  the catalog/config root, not from process CWD.
- Worktree creation can fail on dirty branch/name collisions. Preserve the app's
  cleanup/preservation semantics explicitly.
- Direct `boot-exec` is not the same as managed session attach. Native TUI
  direct exec can bypass daemon/session persistence by design.
- `boot-exec` is Claude-TUI-only. It execs into the native Claude PTY runtime
  and rejects Codex/Opencode launch profiles. Catalog launch support for a
  provider does not imply `boot-exec` support — Codex and Opencode are reached
  through managed sessions (`mux launch`). See
  `docs/adr/0039-boot-exec-claude-only-scope.md`.
- Provider support and provider polish are separate. The shared layer can carry
  NativeFiles/BootDirOverlay even when a provider-specific skill compiler is not
  implemented yet.
- Nanite CLI launch regressions should be treated as exposed latent bugs, not
  proof the shared launch model is wrong. Isolate failing path assumptions:
  workdir, bootdir, stdin/first-turn, env, and provider argv.

## When To Ask For Alignment

Pause and ask the user before merging if:

- preserving existing Torque/Nanite behavior requires keeping a local launch
  path beside the shared path;
- app-specific context assembly does not map cleanly to boot prompt,
  `NativeFiles`, or `BootDirOverlay`;
- a provider's native behavior conflicts with the shared runtime matrix;
- a task would move chat/business logic into `go-agent-launch` or
  `go-agent-context`;
- secrets would be persisted in launch plans or boot dirs.

