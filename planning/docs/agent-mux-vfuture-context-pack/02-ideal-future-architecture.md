# Ideal Future Architecture

## Top-level stack

### Agent Mux
Runtime/session substrate.

Responsibilities:

- launch and manage sessions
- PTY and API-backed provider runtimes
- live process ownership
- attach/detach
- send input/messages
- workspace creation/materialization
- sandbox/policy adapters
- checkpoints and handoffs
- hot/cold transitions
- brokered inter-session messaging
- event streaming and session observability

### Clockwork
Planning and operational orchestration layer.

Responsibilities:

- epics, sprints, tasks, queue state
- dispatch and assignment
- retries and escalation
- hot/cold policy decisions
- logical-agent scheduling
- operational agents: project manager, operator, reviewer dispatcher, stalled-run inspector

### Nanite
Interactive human-facing layer.

Responsibilities:

- chat UX
- session browsing/attach
- tool-centric workflows
- context review
- operator collaboration
- human-in-the-loop interactions

### Agent Ops
Asset/catalog/install layer.

Responsibilities:

- agent definitions
- skills
- tool catalogs
- provider registries
- installer contracts/adapters
- import/export across ecosystems
- local/global/project scope resolution
- search and compatibility across formats such as NANITE.md, CLAUDE.md family, AGENTS.md, and related registries

### Vanta memory
Context and memory primitive.

Responsibilities:

- shared service mode with API/MCP/CLI/etc.
- embedded library mode within Go apps
- namespaced memory scopes
- retrieval/provenance/confidence
- operational memory for warm agents and app-local memory for isolated use cases

### Cerberus
Service/process/devops surface.

Responsibilities:

- process/service lifecycle
- plist/service installs
- dev server supervision
- builds/deploys/hosting workflows
- future operational glue for local services

## Important architectural boundaries

- Agent Mux must not become Clockwork.
- Agent Mux must not become Nanite.
- Agent Mux must not absorb Agent Ops.
- Vanta must remain a primitive available to all apps.
- Cerberus remains a process/service manager, not the semantic owner of agent sessions.

## Entity model

### LogicalAgent
Durable identity.

Fields include:

- id
- role
- responsibilities
- capabilities
- memory scopes
- policies
- permitted tools
- escalation rules
- checkpoint policy
- hot/cold policy

### RuntimeSession
Ephemeral execution instance.

Fields include:

- session id
- logical agent id
- provider runtime type
- workspace root
- live state
- attachability
- health
- active context metrics

### Checkpoint
Durable continuity record.

Fields include:

- checkpoint id
- logical agent id
- task/workflow id
- current status
- completed work
- pending work
- key decisions
- referenced artifacts
- summarized context
- next recommendation

### BrokerEnvelope
Structured inter-session or human-to-session message.

Fields include:

- envelope id
- sender
- recipient
- workflow id
- correlation id
- message type
- priority
- payload
- timestamps
- audit metadata

### CapabilityAsset
An Agent Ops concept, but Agent Mux should be able to consume references to them.

Fields include:

- asset id
- type
- source
- version
- scope
- compatibility metadata
