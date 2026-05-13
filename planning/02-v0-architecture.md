# v0 Architecture

## Top-level model

Agent Mux should be implemented as a local application with a clean internal service boundary.

```text
CLI / TUI / HTTP / MCP / other apps
              |
              v
         Application API
              |
  +-----------+-----------+
  |           |           |
  v           v           v
Catalog   Launch Resolver Session Runtime
  |           |           |
  v           v           v
Config     Workspace    Provider Adapters
Schema     Manager      + PTY + Policy
  |           |           |
  +-----------+-----------+
              |
              v
            Store
           SQLite
```

## Core modules

### 1. Catalog

Responsible for loading and merging:

- global config
- project config
- agent profiles
- role definitions
- skill definitions
- boot fragments
- provider definitions
- workflow definitions
- sandbox profiles

Responsibilities:

- file discovery
- path resolution
- inheritance / override merge rules
- validation
- normalized in-memory model

### 2. Launch Resolver

The most important part of the system.

Inputs:

- project id
- agent id
- provider id
- workflow id or direct launch profile
- optional overrides

Outputs:

- resolved launch plan

A launch plan contains:

- resolved project roots
- resolved workspace strategy
- resolved provider command and args
- resolved environment
- resolved boot prompt text
- resolved readable/writable roots
- resolved policy profile
- session metadata template

### 3. Workspace Manager

Responsible for creating session homes.

Session workspace layout:

```text
workspaces/
  <session-id>/
    inbox/
    execution/
    scratch/
    artifacts/
    prompts/
    state/
    logs/
```

The workspace manager should support strategies:

- `repo-only`
- `workspace-only`
- `git-worktree`
- `hybrid` (repo + workspace tracking root)

For v0, `hybrid` is probably the default.

### 4. Session Runtime

Responsible for:

- process spawn
- PTY attach/detach
- stdout/stderr capture
- session lifecycle
- state transitions
- exit handling
- artifact indexing
- heartbeat / liveness

Session states:

- `created`
- `resolving`
- `ready`
- `launching`
- `running`
- `blocked`
- `completed`
- `failed`
- `killed`
- `archived`

### 5. Provider Adapters

Each provider adapter converts a launch plan into an actual command execution.

Examples:

- `claude-code`
- `codex-cli`
- `gemini-cli`
- `nanite`
- `custom-shell`

Adapter responsibilities:

- command and argument assembly
- env injection
- stdin bootstrap strategy
- prompt file handling
- tool config injection when needed

### 6. Sandbox / Policy Layer

This should be an adapter interface, not a single mechanism.

Targets:

- macOS seatbelt wrapper path for dev/PoC
- Linux bubblewrap path later
- no-sandbox mode for debugging

Policy model should express:

- readable roots
- writable roots
- env allow/deny
- network access
- temp paths
- shared metadata exceptions

### 7. Broker

Optional in early v0, but the interface should exist.

Purpose:

- route inter-agent messages
- support request/reply semantics
- fan out events
- let a primary session coordinate sibling sessions

This should be local and brokered by the runtime, not direct peer sockets in v0.

### 8. Store

SQLite-backed state for:

- sessions
- launch plans
- artifacts
- messages
- workflow runs
- provider runs
- policy denials
- attachments / references

## Internal package sketch

```text
internal/
  app/
    service.go
  catalog/
    loader.go
    merge.go
    model.go
  config/
    schema.go
    validate.go
  launch/
    resolver.go
    plan.go
  workspace/
    manager.go
    layout.go
  session/
    runtime.go
    state.go
    pty.go
  provider/
    provider.go
    claudecode/
    codex/
    gemini/
    nanite/
  sandbox/
    policy.go
    runner.go
    macos/
    linux/
  broker/
    bus.go
    message.go
  store/
    sqlite/
  tui/
    app.go
  api/
    http.go
    mcp.go
```

## API shape

The app should expose a local service layer even before HTTP exists.

Suggested application service methods:

- `ListProjects()`
- `ListAgents(projectID)`
- `ResolveLaunch(input)`
- `LaunchSession(plan)`
- `LaunchWorkflow(workflowID)`
- `ListSessions(filter)`
- `GetSession(sessionID)`
- `AttachSession(sessionID)`
- `StopSession(sessionID)`
- `ArchiveSession(sessionID)`
- `SendMessage(message)`

## TUI shape

Primary screens:

- project picker
- agent picker
- launch preview
- session list
- session detail
- workflow launch view
- artifacts/logs view

## Key design decisions

### Decision 1: no logic in the UI

The TUI should only call the application API.

### Decision 2: launch plan is inspectable

Users and agents should be able to preview the exact prompt, paths, and policy before launch.

### Decision 3: session ID is first-class

Everything hangs off a durable session ID.

### Decision 4: workspace isolation complements worktrees

Do not assume worktrees are sufficient.

### Decision 5: provider-neutral session model

All provider clients should map into the same internal session lifecycle.

