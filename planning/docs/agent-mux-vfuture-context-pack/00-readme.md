# Agent Mux — vFuture Context Pack

This pack is intended for a local coding agent working on the next evolution of Agent Mux.

## Purpose

Agent Mux is the runtime and session control plane for agentic systems.

It is not a planner, not a chat UI, and not an asset installer. It is the execution substrate that other systems use:

- **Nanite** for interactive chat and human-facing agent UX
- **Clockwork** for planning, scheduling, queues, and operational orchestration
- **Agent Ops** for agent/skill/tool catalogs, installers, registries, and compatibility
- **Vanta memory** as a shared or embedded memory primitive
- **Cerberus** as a service/process manager and operational surface for local services

## Current direction

The current v0 is already aligned with the right shape:

- config/catalog
- launch resolution
- provider adapters
- workspace materialization
- PTY session runtime
- SQLite state
- service facade

The next step is to turn it into a durable local runtime with attach/detach, checkpoint/handoff, brokered messaging, and a stable client API.

## Guiding architecture

- Agent Mux runs
- Clockwork decides
- Nanite interacts
- Agent Ops defines and installs
- Vanta informs
- Cerberus manages services and process surfaces

## Deliverable target for the next implementation phase

Build the minimal irreversible foundation for v0.0.2:

- persistent local daemon/runtime
- live session ownership
- attach/detach
- send input
- provider contract that supports both CLI and API runtimes
- logical agent identity separate from runtime sessions
- checkpoint/handoff skeleton
- broker/mailbox skeleton
- local API boundary for Nanite/Clockwork/CLI/TUI clients
