# Workspace parity with the tmux team kit

Parity means an agent can use the intended editable checkout, receive its role
and current assignment, run with the intended environment and MCP grants, and
read the declared project/team files under the launch's permission policy.
The process CWD, boot directory and editable checkout may be different paths.

This comparison uses the authored `agent-os/team/bin/team` launcher and Tether's
catalog launch path (`internal/launch/resolver.go`, `internal/app/agent_ops.go`,
`internal/app/session_lifecycle.go`, `internal/app/shared_launch.go` and
`internal/workspace/workroot.go`), as of 2026-10-09. It describes source behavior;
installed catalogs and the deployed binary can differ.

| Area | tmux team kit | Tether launch / action |
|---|---|---|
| Working directory and worktree | Starts in a Cairn boot directory; grants the project, team and worktrees directories. Task agents choose an isolated checkout themselves. | `shared`/`hybrid` use `repo_root`; `worktree`/`isolated` materialize `<base>/repo` from source `HEAD`. Provider projection points at `work_root` (Codex `--cd`, Claude/Antigravity `--add-dir`). This change makes literal slash branch names honor `worktree_name` instead of silently detaching. |
| Environment | Exports `TEAM_*`, a user tool `PATH`, and per-agent `TMPDIR`/`GOTMPDIR`; Codex explicitly injects these into shell commands. | Inherits the **daemon's** environment in merge mode, or selected keys in whitelist mode, then applies launch overrides. Team variables and tool/temp paths need explicit launch configuration; Codex shell policy can be supplied through provider flags. Client shell values do not automatically cross the API. |
| Boot prompt/profile | Cairn plants role instructions; the launcher adds identity, mission/brief and assignment paths. | Catalog fragments, agent instructions, `--boot-profile`, `--boot-prompt` and `--prompt-append` compose the prompt. Boot-profile identity is refreshed to the materialized work root. Supply the same role/assignment content through these inputs and non-secret injection; Tether does not infer a team role from its name. |
| MCP configuration | Uses Cairn's planted provider configuration. | Tether plants its gateway into the provider boot directory and validates project/launch/boot-profile allowlists against enabled upstreams. Configure equivalent grants explicitly; a matching server name alone does not establish equivalent tool access. |
| Scope and files visible | Scope is a project path, supplied to Cairn and provider `--add-dir`, alongside team/worktrees paths. | Project `repo_root` selects the checkout; provider flags can add team paths when the sandbox policy permits. Native-file and boot-directory injection plant role/skill/context files. A prompt mentioning a path grants no filesystem access. Isolated worktrees contain committed source, not the source checkout's uncommitted or ignored files. |

For a task checkout, set `workspace.mode: worktree` and a unique literal
`workspace.worktree_name: task/<task-id>`. An empty name creates a detached
worktree; `{{…}}` names remain reserved and also create a detached worktree.

Boot workspaces must be outside protected `~/.tether` and the state database's
directory. The starter catalog and Tether example now use `~/tether/workspaces`
(and `~/tether/tmp` for temp files). Existing explicit project `session_root`
values take precedence over global defaults and are not migrated. An owned
launch can set `workspace.write_home` to a safe location; that wins over the
project value, and launch files are refreshed without restarting the daemon.
Changing global workspace defaults alone cannot repair a stale project override.

Worktree naming templates remain in CW-20260515-0144; automatic Cairn/team-profile
import is filed as CW-20261009-0013. Native session/workspace reuse on resume
belongs to tasks 0005/0018;
this change leaves that lifecycle with its owner.

Existing setup surfaces: [catalog launches](catalog-launch-profiles.md),
[boot profiles](launches/bootgen.md) and
[native-file / boot-directory injection](shared-agent-launch-reference.md).
Inherited environment values are not persisted in the plan; explicit overrides
and injection content are. Keep those inputs non-secret. Daemon-managed terminal
sessions retain work products until an explicit operator cleanup.
