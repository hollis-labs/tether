# Config Schema v0

## Goals

The config model should separate identity, environment, and execution concerns.

Recommended document types:

- global catalog config
- project profile
- agent profile
- launch profile
- workflow profile
- sandbox profile

## 1. Global catalog

```yaml
version: 0.1.0
catalog:
  roots:
    roles: ~/.nanite/roles
    skills: ~/.nanite/skills
    agents: ~/.nanite/agents
    providers: ~/.nanite/providers
    sandboxes: ~/.nanite/sandboxes
    workflows: ~/.nanite/workflows
  defaults:
    workspace_root: ~/agent-mux/workspaces
    state_db: ~/agent-mux/state/agent-mux.db
    temp_root: ~/agent-mux/tmp
```

## 2. Project profile

```yaml
id: clockwork-manifold
name: Clockwork Manifold
repo_root: ~/Projects-apps/clockwork-manifold
tracking_root: ~/Projects-apps/agent-workspaces/execution/clockwork-manifold
knowledge_base:
  - ~/Projects-apps/agent-workspaces/knowledge/projects/clockwork-manifold.md
boot_fragments:
  - ~/.nanite/boot/common.md
  - ~/.nanite/boot/projects/clockwork-manifold.md
workspace:
  default_mode: hybrid
  worktree_base: ~/Projects-apps
  session_root: ~/agent-mux/workspaces/clockwork-manifold
```

## 3. Agent profile

```yaml
id: backend-auditor
name: Backend Auditor
roles:
  - backend
  - auditor
  - go
skills:
  - doc-search
  - qstatus
  - go-test
  - go-build
context_files:
  - .nanite/agents/backend-auditor.md
boot_fragments:
  - ~/.nanite/boot/agents/backend-auditor.md
permissions:
  network: true
  default_sandbox: macos-dev-tight
```

## 4. Provider profile

```yaml
id: claude-code
type: cli
command: claude
args: []
bootstrap:
  mode: stdin
  prompt_prefix: ""
env:
  passthrough:
    - HOME
    - PATH
    - SHELL
    - ANTHROPIC_API_KEY
```

## 5. Launch profile

```yaml
id: orch-mcp-hardening
project: clockwork-manifold
agent: backend-auditor
provider: claude-code
workspace:
  mode: git-worktree
  worktree_name: orch-mcp-hardening
  write_home: ~/agent-mux/workspaces/clockwork-manifold/orch-mcp-hardening
prompt:
  include_project_boot: true
  include_agent_boot: true
  include_knowledge_base: true
sandbox:
  profile: macos-dev-tight
overrides:
  env:
    CLOCKWORK_SESSION_ROLE: backend-auditor
```

## 6. Workflow profile

```yaml
id: multiplexor-hardening-pass
description: Planner plus parallel specialists plus reducer
entry:
  type: parallel
  sessions:
    - id: planner
      launch: planner-pass
    - id: backend
      launch: backend-pass
    - id: reviewer
      launch: reviewer-pass
  reducer:
    mode: brokered-summary
    target: planner
```

## 7. Sandbox profile

```yaml
id: macos-dev-tight
network: true
read_roots:
  - ~/Projects-apps
  - ~/agent-mux
  - ~/.nanite
write_roots:
  - ${workspace.write_home}
  - ${project.repo_root}/.git
  - ${project.tracking_root}
  - /tmp
  - /private/tmp
env:
  passthrough:
    - HOME
    - PATH
    - SHELL
    - TMPDIR
  deny:
    - AWS_SECRET_ACCESS_KEY
```

## Merge rules

1. Global defaults load first.
2. Project profile overlays defaults.
3. Agent profile overlays project defaults where appropriate.
4. Launch profile sets final concrete runtime values.
5. Explicit CLI/TUI overrides win last.

## Validation rules

- IDs must be stable and slug-safe.
- Paths must expand to absolute paths before runtime.
- Provider profiles must declare a concrete command.
- Sandbox write roots must be explicit.
- A launch profile must fully resolve to one provider, one agent, and one project.
- Workflow session IDs must be unique within the workflow.

## Recommendation

Use YAML for authoring, but validate against a JSON Schema or strongly typed Go structs with strict decode and validation.

