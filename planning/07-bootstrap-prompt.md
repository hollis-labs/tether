You are building the first useful version of **Agent Mux**, a local-first agent launcher and session control plane.

Read the full context packet before writing code.

Files to read first:

- `00-README.md`
- `01-product-vision.md`
- `02-v0-architecture.md`
- `03-config-schema-v0.md`
- `04-messaging-and-multiplexor.md`
- `05-mvp-scope-and-milestones.md`
- `06-local-agent-task-list.md`

## Mission

Implement a rough but functional Go-based CLI/TUI application that can:

- load project, agent, provider, launch, and workflow configs
- resolve a launch plan
- create a workspace for a session
- launch a provider client inside a PTY
- persist session state in SQLite
- list and inspect sessions
- optionally launch a simple parallel workflow

## Architecture rules

- Agent-first architecture
- TUI/CLI are consumers, not the core
- Put logic behind an application service layer
- Keep config loading, resolution, workspace management, runtime, and store separate
- Do not bury orchestration logic inside the UI
- Launch plans must be inspectable before execution

## Build preferences

- Use Go
- Prefer Cobra for CLI
- Prefer Bubble Tea for TUI if it helps; keep the first version narrow
- Use SQLite for local persistence
- Use PTY for launched sessions
- Keep dependencies minimal but practical

## Product stance

This is not just a shell launcher.
It is a control plane for local agent sessions.

## Suggested first steps

1. Scaffold the repo and package boundaries.
2. Implement typed config models and YAML loading.
3. Implement launch plan resolution.
4. Implement workspace creation.
5. Implement session runtime for one provider adapter.
6. Implement SQLite persistence.
7. Add minimal CLI flows.
8. Add a minimal TUI shell if time allows.

## Expected output style

- Be concrete.
- Make crisp implementation choices.
- Leave short architecture notes when a decision matters.
- Do not over-engineer sandboxing or GUI in the first pass.

## Important bias

Optimize for a working v0 that is structurally correct, not for polish.

