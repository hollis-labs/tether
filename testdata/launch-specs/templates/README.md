# Common-setup templates (S5)

These are the reusable common-setup templates for the canonical
`tether.launch` LaunchSpec (`../launch-assembly.yaml`).

A template here is a partial input bag: the recurring
`runner` + `isolation` combinations that the legacy
`~/.tether/catalog/launches/` grid repeated 64 times. A concrete launch
bag (`../launches/*.yaml`) supplies the project-specific knobs
(`work_dir`, `project`, `agent`, `agent_role`) and is understood as
"this template, plus those values."

The legacy catalog encoded the runner axis as the launch filename suffix
(`-claude`, `-claude-stream`, `-codex-launch`, `-codex-app-server`,
`-opencode`, `-tui`) and the isolation axis as the `.worktree` filename
twin. Both axes are now input values — these templates name the common
combinations once.

| Template | runner | isolation | Replaces legacy suffix |
|---|---|---|---|
| `claude-shared.yaml`          | claude-code      | hybrid   | `-claude` |
| `claude-worktree.yaml`        | claude-code      | worktree | `-claude-worktree` |
| `claude-stream.yaml`          | claude-stream    | hybrid   | `-claude-stream` |
| `claude-stream-worktree.yaml` | claude-stream    | worktree | `-claude-stream-worktree` |
| `claude-tui.yaml`             | claude-pty       | hybrid   | `-claude-tui` |
| `codex-cli.yaml`              | codex-cli        | hybrid   | `-codex-launch` |
| `codex-app-server.yaml`       | codex-app-server | hybrid   | `-codex-app-server` |
| `opencode.yaml`               | opencode         | hybrid   | `-opencode` |

**The `.worktree` twins are gone.** A worktree launch is the shared
template with `isolation: worktree` — there is no second file. Only two
worktree templates ship here as conveniences for the two highest-traffic
runners; any other runner gets worktree isolation by setting
`isolation: worktree` directly in the bag.
