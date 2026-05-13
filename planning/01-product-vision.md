# Product Vision

## One-line definition

A local-first agent launcher and session control plane for managing profiles, workspaces, prompts, policies, and multi-session execution.

## What the product is

Agent Mux is a runtime that resolves a launch request into a reproducible local session.

A launch request becomes:

- selected project context
- selected agent profile
- selected provider adapter
- selected workspace strategy
- selected sandbox/policy profile
- resolved boot prompt
- session metadata and lifecycle tracking

## What the product is not

It is not:

- just a shell alias manager
- just a prompt composer
- just a worktree helper
- just a task runner
- a replacement for Nanite or Clockwork

It should compose with those systems.

## Primary user stories

### 1. Launch a single high-quality session quickly

A user wants to pick:

- project
- agent profile
- provider client
- workspace mode
- optional boot fragments

Then start a session with one command or one TUI flow.

### 2. Resume or inspect prior sessions

A user wants to see:

- active sessions
- ended sessions
- provider used
- workspace path
- boot prompt used
- artifacts produced
- exit status

### 3. Run parallel top-level sessions safely

A user wants 2–4 peer sessions working on one plan without stepping on each other.

### 4. Feed the same runtime from multiple consumers

Consumers may include:

- CLI
- TUI
- local HTTP API
- MCP server
- Nanite
- Clockwork
- future GUI

### 5. Support agent-to-agent coordination

A user wants a primary session to coordinate with sibling sessions without relying on brittle file polling.

## Product boundaries

### In scope for v0

- local catalog loading
- config resolution
- boot prompt composition
- session workspace creation
- PTY-backed session launch
- basic sandbox/policy adapter hooks
- session tracking in SQLite
- multi-session launch plan execution
- optional brokered message bus skeleton

### Out of scope for v0

- full GUI
- cloud sync
- marketplace/distribution platform
- full observability suite
- generic secure GUI sandboxing
- arbitrary remote orchestration
- advanced cost accounting

## Product philosophy

### Agent-first

Everything should be usable without the TUI.
The TUI is a client, not the core.

### Explicit over implicit

A launch plan should be inspectable before execution.

### Policy attached to session

Permissions are not a vague concept. They are attached to the resolved launch plan.

### Workspace as unit of isolation

Every session should have a home directory and tracking root, even when the repo is shared.

### Broker, not peer mesh

If inter-agent communication exists, route it through the runtime so it can be logged, gated, and replayed.

