# LaunchBags — one per legacy launch (S5)

This directory holds **one `LaunchBag` per concrete legacy launch**: 64
bags reproducing every `~/.tether/catalog/launches/*.yaml`, plus
`tether-minimum.yaml` (the minimum-config demo bag, not a legacy launch).

Each bag carries:

- `spec: tether.launch` — names the canonical `../launch-assembly.yaml`.
- `name:` — the legacy launch `id`, verbatim.
- `inputs.work_dir` — the matching project's `repo_root`
  (`~/.tether/catalog/projects/<project>.yaml`).
- `inputs.runner` — mapped from the legacy `provider`
  (`claude-code` / `claude-stream` / `claude-pty` / `codex-cli` /
  `codex-app-server` / `opencode`).
- `inputs.isolation` — `hybrid` or `worktree`, from the legacy
  `workspace.mode`. **The legacy `.worktree` twin file is gone**: the
  worktree launch is the same bag with `isolation: worktree`, not a
  separate spec or file.
- `inputs.project` / `inputs.agent` — the legacy `project` / `agent`.
- `inputs.agent_role` — the role-file path stem (under
  `~/.nanite/roles/`) the `agent_summary` var reads, derived from the
  matching boot-profile's `agent` slot `path`.

The legacy `prompt.include_project_boot` / `prompt.include_agent_boot`
booleans map to the spec's `include_project_boot` / `include_agent_boot`
inputs (both default `true`; every legacy launch set both `true`, so the
bags rely on the spec default rather than restating it).

## Notes on the mapping

- **agent-mux launches** carry legacy `project: tether` (the legacy
  files literally say so) — the bags preserve that. `work_dir` is the
  tether `repo_root` accordingly.
- **Launches with no matching boot-profile** (e.g. `agent-mux-claude`,
  `nanite-claude-stream`, `tesseract-claude-stream`, `torque-claude-stream`,
  the codex-app-server variants) fall back to the spec-default role
  `domain/backend/worker`. Their header comment records this.
- **`stack-explorer-auditor-codex`** has no auditor-specific boot-profile;
  it uses the `stack-explorer` backend role.
