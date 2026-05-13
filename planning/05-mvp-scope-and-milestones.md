# MVP Scope and Milestones

## MVP objective

Ship a rough but useful local binary that reliably launches and manages agent sessions.

## Must-have capabilities

### Milestone 1 — config + catalog

- define strict config structs
- load global/project/agent/launch/workflow docs
- validate and normalize paths
- list projects, agents, providers

### Milestone 2 — launch resolver

- resolve a launch request into a concrete launch plan
- compose boot prompt from fragments
- preview resolved plan in JSON or markdown

### Milestone 3 — workspace manager

- create session directory layout
- support hybrid workspace mode
- persist session metadata

### Milestone 4 — session runtime

- launch provider command in PTY
- capture logs and exit status
- attach/detach from session
- list active sessions

### Milestone 5 — TUI shell

- picker flow for project/agent/provider
- launch preview screen
- active sessions screen
- session detail view

### Milestone 6 — parallel workflow launch

- launch 2–4 sessions under one workflow ID
- track them together
- stop or inspect each one

### Milestone 7 — broker skeleton

- store messages
- send request/reply between sessions via API
- simple CLI commands to inspect and post

## Nice-to-have after MVP

- HTTP API
- MCP adapter
- sandbox profile execution adapters
- session archive UI
- artifact index/search
- boot prompt diffing
- run templates

## Explicit non-goals for MVP

- polished GUI
- production-grade sandboxing on every platform
- generalized remote orchestration
- full autonomous swarm behavior

## Suggested order

1. catalog
2. resolver
3. workspace
4. runtime
5. CLI commands
6. TUI
7. workflow launcher
8. broker skeleton

## Suggested initial commands

- `mux projects list`
- `mux agents list --project <id>`
- `mux resolve --launch <id>`
- `mux launch --launch <id>`
- `mux sessions list`
- `mux sessions attach <session-id>`
- `mux workflow run <id>`
- `mux messages send ...`
- `mux messages list --session <id>`

