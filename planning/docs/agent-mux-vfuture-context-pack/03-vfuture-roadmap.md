# vFuture Roadmap

## End-state themes

- local-first runtime substrate
- provider-agnostic execution
- attach/detach everywhere
- hot and cold logical agents
- brokered multi-session collaboration
- clean integration with Nanite, Clockwork, Agent Ops, Vanta, Cerberus
- maintainable OSS-friendly package boundaries
- agent-first/service-first architecture from day one

## v0.0.2 — Runtime foundation

Must include:

- long-lived local daemon/runtime
- durable live session ownership
- list/get/stop session via API
- live attach stream
- send input to session
- CLI adapter remains supported
- API runtime adapter interface is introduced
- LogicalAgent separated from RuntimeSession in storage/modeling
- basic Checkpoint model
- basic Broker/Mailbox model
- local API transport (HTTP or Unix socket or both)
- event bus/internal event stream
- lock-safe concurrency on in-memory runtime maps/state
- env handling that merges passthrough plus overrides, not blind replacement

Should not include yet:

- rich desktop GUI
- deep orchestration UI
- full planner/operator agents
- exhaustive asset-system adapters
- platform-perfect sandboxing

## v0.0.3 — Integration foundation

- Nanite can discover and attach to Agent Mux sessions
- Clockwork can launch and inspect sessions through Agent Mux
- detached interactive sessions behave predictably
- initial checkpoint resume path
- initial mailbox/request-reply semantics
- first API-backed provider implementation
- event subscription support

## v0.1 — Operational agent support

- hot/cold lifecycle policy
- planner/operator logical-agent support
- queue review hooks from Clockwork
- stalled session detection
- escalation conditions
- memory scope integration via Vanta
- warm knowledge keeper prototype

## v0.2 — Agent Ops integration

- asset search/resolve contract
- installer adapter model
- local/global/project scope resolution
- import/export across multiple agent ecosystems
- registry/catalog integration

## v0.3+ — Rich UX and advanced orchestration

- richer TUI
- optional desktop GUI
- workflow visualization
- multiplexor flows
- background agents with brokered collaboration
- observability dashboards
- better service-manager integration with Cerberus
