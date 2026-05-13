# Implementation Guidelines

## Architectural rules

1. Keep package boundaries narrow and explicit.
2. Avoid god objects and giant service structs that absorb unrelated responsibilities.
3. Prefer small interfaces at the consumption boundary, not broad abstraction everywhere.
4. Make runtime session state durable in storage and operational ownership explicit in the daemon.
5. Separate durable identity from ephemeral execution.
6. Make inter-session messaging brokered and auditable.
7. Design for multiple clients from day one.
8. Keep Agent Mux runtime concerns separate from Agent Ops asset concerns.

## Go project hygiene

### Tooling
The project should assume and document a standard Go developer toolchain:

- current stable Go version
- gopls
- gofmt
- goimports
- golangci-lint
- staticcheck
- govulncheck
- gotestsum or equivalent for readable test runs
- make targets or task runner targets for common workflows

### Repository quality
Include and enforce:

- Makefile or task targets
- lint target
- test target
- fmt target
- vet target
- vuln target
- CI-friendly commands
- clear package readmes where complexity grows

### Code quality
- no hidden global mutable state
- no giant app package that owns everything forever
- no mixed UI/runtime logic
- no unbounded interfaces that hide concrete behavior
- explicit error wrapping and classification
- context.Context used appropriately at API/runtime boundaries
- lock shared state correctly
- prefer event-driven state changes over incidental shared memory mutation

## Reuse before rebuilding

Before introducing a new package or custom implementation:

1. Check existing internal packages first.
2. Check community packages second.
3. Only build custom when necessary for fit, clarity, or control.

Areas to evaluate before rebuilding:

- provider runners
- envelopes/messages/brokers
- queue primitives
- process supervision helpers
- config loading/validation
- event bus abstractions
- service lifecycle helpers

## Internal package alignment

The local agent should inspect existing packages and applications for reuse and consistency, especially:

- Nanite
- Clockwork
- Vanta memory
- Cerberus
- go-providers
- brokers/envelopes/messages packages
- go-queue
- any local service or runtime scaffolding already in use

Do not duplicate capabilities that already exist in a clean reusable package.

## Runtime design principles

- service-first, client-agnostic
- local-first
- explicit lifecycle transitions
- deterministic enough for orchestration, flexible enough for interactive use
- CLI and API runtimes share the same session model
- checkpoints and handoffs are first-class
- event streams are a primitive, not a later bolt-on

## Storage guidance

SQLite is appropriate for local durable state. Ensure clear tables and models for:

- logical_agents
- runtime_sessions
- checkpoints
- broker_envelopes
- artifacts
- event_log
- client_attachments

Keep schema evolvable. Use migrations.

## API guidance

Expose a minimal but stable local API for clients. Possible initial operations:

- create session
- launch session
- list sessions
- get session
- attach stream
- send input
- stop session
- create checkpoint
- list checkpoints
- send broker message
- list broker messages/events

The local API can begin as HTTP or Unix socket, but the boundary must be clear enough for Nanite and Clockwork to adopt later.
