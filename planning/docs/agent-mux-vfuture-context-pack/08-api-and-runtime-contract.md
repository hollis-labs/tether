# API and Runtime Contract Sketch

## Runtime lifecycle

Agent Mux should become a long-lived local service.

### Clients
- CLI
- TUI
- Nanite
- Clockwork
- future MCP bridge
- future desktop GUI

### Runtime responsibilities
- own active sessions
- expose lifecycle operations
- preserve detached session semantics
- publish events
- manage broker/mailbox
- preserve checkpoint continuity

## Candidate local API

### Session operations
- `POST /sessions`
- `POST /sessions/{id}/launch`
- `GET /sessions`
- `GET /sessions/{id}`
- `POST /sessions/{id}/stop`
- `POST /sessions/{id}/input`
- `GET /sessions/{id}/attach` (streaming)
- `POST /sessions/{id}/detach` (may be mostly client semantic)

### Checkpoint operations
- `POST /sessions/{id}/checkpoint`
- `GET /logical-agents/{id}/checkpoints`
- `POST /logical-agents/{id}/resume`

### Broker operations
- `POST /broker/envelopes`
- `GET /broker/envelopes`
- `GET /broker/envelopes/{id}`
- `POST /broker/envelopes/{id}/reply`

### Event operations
- `GET /events/stream`
- `GET /sessions/{id}/events`

## Provider contract

A common provider runtime contract should support both CLI and API execution modes.

Suggested high-level behaviors:

- prepare
- start
- stop
- send input/message
- attach/read stream if interactive
- checkpoint hints/metrics if available
- report health/state

CLI runtimes and API runtimes should both fit the same higher-level session contract, even if their internals differ.

## Attach/detach semantics

### Interactive attached session
Client is actively observing and optionally sending input.

### Detached running session
Runtime retains ownership; client disconnect does not kill the session.

### Background session
A session may continue operating because the task itself is active or the runtime/broker continues to feed it work.

### Reattach
A client can reconnect to live output and state later.

## Broker semantics

Do not implement direct PTY-to-PTY free-form chatter.
Use structured envelopes:

- request
- response
- notice
- escalation
- handoff
- status update
