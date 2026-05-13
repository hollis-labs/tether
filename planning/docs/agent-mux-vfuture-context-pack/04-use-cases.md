# Example Use Cases

## 1. Interactive attached coding session

A user starts a Claude Code or other CLI-backed session through Agent Mux. Nanite or the terminal attaches to that live session. The user can detach without terminating it and reattach later from another client.

Requirements:

- live PTY ownership
- attach/detach
- send input
- log/event capture

## 2. Detached background long-running task

A logical agent performs a larger task in the background. The session is detached but still active. Clockwork can inspect status, and a human can reattach later if needed.

Requirements:

- persistent runtime
- detached session lifecycle
- event status
- checkpoint support

## 3. Agent Multiplexor Pattern

A primary session delegates to two to four sibling sessions. Communication is brokered through Agent Mux, not via ad hoc files or PTY chatter. Replies are correlated and auditable.

Requirements:

- broker/mailbox
- correlation IDs
- workflow-scoped session groups
- request/reply semantics

## 4. Warm knowledge keeper

A persistent or semi-persistent logical agent maintains living operational understanding of the system:

- canonical repo locations
- aliases and drift
- active conventions
- recently used paths and libraries
- relationship map across projects/apps

This agent consults Vanta memory, recent system activity, and canonical registries. It should not become the sole source of truth.

Requirements:

- logical-agent identity
- memory scope access
- checkpointing
- policy-aware warm/cold transitions

## 5. Clockwork project manager / planner

A manager agent reviews epics, sprints, tasks, and queues, and dispatches specialized worker agents for missing context, enrichment, or bounded execution. It should coordinate, not perform all work directly.

Requirements:

- logical agents
- Clockwork integration boundary
- dispatch hooks
- broker/mailbox
- access to procedures/policies and relevant memory scopes

## 6. Architecture ideation partner

A warm strategic agent collaborates with the user to shape designs, recognizes existing internal packages and primitives, and produces context packets for downstream planning agents.

Requirements:

- memory scopes for user goals and system philosophy
- Vanta integration
- context packet generation
- clean handoff to Clockwork/Nanite workflows

## 7. Content ideation and writing partner

A durable agent with access to style preferences, covered topics, and writing memory helps shape topics and produce structured drafts or downstream writing packets.

Requirements:

- memory scopes
- checkpoint/handoff
- catalog lookup for prior content and preferences

## 8. API-backed non-CLI execution

For non-terminal tasks or vendor-independent operation, Agent Mux launches API-based sessions using provider SDKs while preserving the same session model as CLI runtimes.

Requirements:

- common provider session contract
- streaming abstraction
- checkpoint compatibility
