# Local Agent Task List

## Mission

Build the first useful version of Agent Mux as a Go-based CLI/TUI application.

## Constraints

- local-first
- CLI/TUI first
- agent-first architecture
- minimal dependencies beyond what materially helps
- rough MVP is acceptable
- clear package boundaries required

## Implementation tasks

### Task 1 — bootstrap repo

- initialize Go module
- add Cobra root command
- add internal package layout
- add Makefile or mage targets
- add README and dev instructions

### Task 2 — define domain models

Implement typed models for:

- project
- agent
- provider
- launch profile
- workflow
- sandbox profile
- resolved launch plan
- session
- message

### Task 3 — config loading and validation

- load YAML files
- normalize paths
- validate required fields
- support layered config resolution

### Task 4 — launch resolver

- resolve merged launch input
- generate boot prompt text
- emit previewable launch plan

### Task 5 — workspace manager

- create session ID
- create workspace layout
- persist prompt copy and metadata

### Task 6 — session runtime

- spawn PTY process
- persist state transitions
- stream output to log file
- support attach/detach

### Task 7 — SQLite store

Create tables for:

- sessions
- launch_plans
n- artifacts
- workflows
- workflow_sessions
- messages
- events

### Task 8 — CLI flows

- list projects/agents/providers
- resolve launch plan
- launch session
- list and inspect sessions
- run workflow

### Task 9 — TUI flows

- launch picker
- launch preview
- session list
- session detail log tail

### Task 10 — broker skeleton

- create message model
- store messages
- add CLI commands to send/list/reply
- wire message policy checks later

## Coding standards

- keep packages small and explicit
- no UI logic in core packages
- no hidden globals for runtime state
- prefer typed structs over map-heavy config flows
- every user-facing action should go through application service layer

## Deliverables expected from the coding agent

1. working repo scaffold
2. initial domain model + config loader
3. session launch path for one provider
4. session persistence
5. basic TUI flow
6. concise architecture notes while implementing

## Definition of done for first pass

A user can:

- define one project
- define one agent
- define one provider
- resolve a launch plan
- launch a PTY-backed session
- see it in the session list
- attach to the session
- optionally launch a simple 2-session workflow

