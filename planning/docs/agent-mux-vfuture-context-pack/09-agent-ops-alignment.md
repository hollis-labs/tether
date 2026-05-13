# Agent Ops Alignment Notes

Agent Mux should not become the owner of agent-system installation formats.

However, Agent Mux must be designed to consume resolved assets that Agent Ops manages.

## Agent Ops responsibilities

- install/update/uninstall agent systems
- manage catalogs and registries
- resolve local/global/project scopes
- adapt multiple file/config conventions
- expose search/lookup for agents, skills, tools, prompts, procedures

## Why this matters

The current agent-workspaces / agentrc concept is intentionally being superseded. The behavior matters more than the old file layout.

The new direction should reduce conflation between:

- session workspaces
- agent assets
- installer-managed configs
- runtime execution state

## Interface expectation

Agent Mux should expect one or more of the following from Agent Ops later:

- resolved agent profile references
- resolved skill/tool/prompt references
- compatibility metadata
- installation status
- lookup/search APIs or local catalogs

Agent Mux can start with local catalog files, but do not overfit the runtime around one historical folder layout.
