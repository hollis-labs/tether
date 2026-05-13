# Agent Mux v0 Context Pack

This pack is a build-start packet for a local coding agent to implement a CLI/TUI-first launcher, workspace manager, and session control plane for agent workflows.

## Working name

- **Agent Mux**
- Alternate internal names: **Mux**, **Agent Control Plane**, **Agent Hub**

## Product intent

A local-first control plane for launching, isolating, and coordinating AI agent sessions across different provider clients and local tools.

Primary targets:

- Claude Code
- Codex CLI
- Gemini CLI / SDK wrappers
- Nanite sessions
- Clockwork-triggered agent runs

This is **not** just a menu that shells out.
It is a session runtime with:

- agent catalog resolution
- boot prompt composition
- workspace provisioning
- sandbox/policy adapters
- PTY/session lifecycle management
- optional multi-session orchestration
- optional inter-agent messaging

## Why this exists

Current pain points:

- hard to remember where profiles, roles, skills, prompts, and configs live
- launching across multiple providers is repetitive and error-prone
- concurrent sessions in the same repo can collide
- worktrees help, but they do not solve everything
- session state, outputs, and handoffs need a first-class home
- there is no single place to inspect, resume, or coordinate local agent runs

## Design stance

- **Agent-first**: human UI is only one consumer of the runtime
- **Local-first**: all core flows work locally without cloud control plane requirements
- **CLI/TUI first**: GUI later, not required for usefulness
- **Provider-agnostic**: launch plans target different clients through adapters
- **Workspace-scoped**: every session gets a clear home and policy envelope
- **Brokered orchestration**: if agents communicate, do it through a broker, not ad hoc peer wiring

## Included docs

- `01-product-vision.md` — product framing and boundaries
- `02-v0-architecture.md` — concrete system architecture
- `03-config-schema-v0.md` — initial config model
- `04-messaging-and-multiplexor.md` — multi-session orchestration + agent messaging model
- `05-mvp-scope-and-milestones.md` — build plan
- `06-local-agent-task-list.md` — implementation backlog for a coding agent
- `07-bootstrap-prompt.md` — boot prompt for the local coding agent

## Key architectural conclusions

1. Build the runtime as a **control plane**, not a wrapper script.
2. Start with **Go + Cobra + Bubble Tea + PTY + SQLite**.
3. Model launch as resolution of:
   - project
   - agent
   - provider
   - workspace
   - policy
   - boot prompt
4. Use **workspace isolation** in addition to worktrees.
5. Keep the system open for **brokered inter-agent messaging** later.
6. Treat GUI as a later consumer of the same runtime API.

## Notes from the existing ecosystem

This pack assumes the following patterns already exist and should influence the design:

- shared role/skill/profile catalogs
- project-local overrides
- boot prompt layering
- workspace-based execution tracking
- parallel digest / multi-agent synthesis patterns
- Nanite and Clockwork composition

## Suggested repo structure

```text
agent-mux/
  cmd/
    mux/
  internal/
    app/
    catalog/
    config/
    launch/
    workspace/
    session/
    sandbox/
    provider/
    broker/
    store/
    tui/
    api/
  schemas/
  examples/
  docs/
```

## First goal

Deliver a rough but functional CLI/TUI that can:

- list projects and agents
- compose a boot prompt
- create a session workspace
- launch a provider client inside a PTY
- track status and outputs
- run 2–4 parallel sessions safely

