# Messaging and the Agent Multiplexor Pattern

## Direct answer

Yes.
Launching multiple sessions through a single runtime creates a natural opportunity for **brokered inter-agent messaging**.

But do not start with raw peer-to-peer messaging.
Use a **local broker** controlled by Agent Mux.

## Why brokered messaging is better

If the runtime owns the sessions, it can also own:

- message routing
- addressing
- audit trail
- replay
- filtering
- policy checks
- delivery status

That is much cleaner than ad hoc file drops or unmanaged sockets between sessions.

## Recommended mental model

Not:

- agent A opens a socket to agent B

Instead:

- agent A sends a message to the broker
- broker validates and routes it
- agent B receives it through its session channel

## Message types

Suggested v0 message envelope:

```json
{
  "id": "msg_123",
  "workflow_id": "wf_001",
  "from_session": "planner",
  "to_session": "backend",
  "type": "request",
  "topic": "analyze-api-surface",
  "body": "Review the task executor interface and return risks.",
  "expects_reply": true,
  "correlation_id": null,
  "created_at": "2026-04-18T12:00:00Z"
}
```

Core message types:

- `request`
- `reply`
- `event`
- `status`
- `artifact-reference`
- `handoff`

## Delivery model options

### v0

Broker stores messages in SQLite and exposes them through the runtime.

Delivery may happen by:

- injecting messages into session stdin as framed text
- making them visible through a session-side helper command
- exposing a local HTTP/MCP endpoint the agent can poll

The cleanest first move is **brokered polling or helper-command retrieval**, not magical stdin mutation during long freeform sessions.

### v1+

Add streaming delivery:

- local websocket
- local SSE
- named pipe
- local unix socket

## Chatting with one agent and having it talk to two others

Yes, structurally that is straightforward.

Example:

1. User talks to `planner`.
2. `planner` sends requests to `backend` and `reviewer` through the broker.
3. `backend` and `reviewer` reply to `planner`.
4. `planner` synthesizes and replies to the user.

That is effectively a **brokered star topology**.

## Important warning

Do not make the first version fully autonomous.

Start with one of these patterns:

### Pattern A: supervised brokered requests

- primary agent requests work
- sibling agents respond
- primary agent synthesizes
- user stays in the loop

### Pattern B: workflow-scripted fan-out/fan-in

- workflow definition launches parallel sessions
- each session runs a bounded instruction
- reducer session combines outputs

Pattern B is the cleanest MVP for your Agent Multiplexor Pattern.

## Suggested v0 capabilities

- launch N sibling sessions under one workflow ID
- send structured messages between sessions through broker API
- list pending messages for a session
- post reply from a session
- attach artifacts or references to messages
- retain full transcript of messages in the store

## What not to do in v0

- unrestricted arbitrary cross-session access
- hidden autonomous self-spawning networks
- implicit file-based coordination as the main path
- direct peer TCP between sessions

## Simple workflow example

```yaml
id: api-hardening-multiplex
entry:
  type: parallel
  sessions:
    - id: planner
      launch: planner-pass
    - id: backend
      launch: backend-pass
    - id: reviewer
      launch: reviewer-pass
message_policy:
  allow:
    - from: planner
      to: backend
    - from: planner
      to: reviewer
    - from: backend
      to: planner
    - from: reviewer
      to: planner
reducer:
  mode: planner-synthesis
```

## Recommendation

Build the interfaces for brokered messaging in v0, but keep the actual UX narrow:

- workflow-scoped messages only
- explicit addressing only
- request/reply only
- audit everything

