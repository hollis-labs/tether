# Multiplexor Demo

Demonstrates the **Agent Mux multiplexor pattern**: a primary agent session
delegates work to sibling sessions through the broker's structured
request/reply envelopes. All sessions share a session group so the workflow
is auditable as a unit.

## Pattern overview

```
Primary ──── POST /broker/requests ──────────→ Sibling1
                (message_type: request)
               ←── POST /broker/envelopes ──────
                (message_type: response,
                 correlation_id: <server-assigned>)

Primary ──── POST /broker/envelopes ─────────→ Sibling2
                (message_type: notice)
```

All three sessions belong to the same `session_group`, queryable via
`GET /session-groups/{id}/sessions`. The request/response thread is
queryable via `GET /broker/envelopes?workflow_id=…&correlation_id=…`.

## Prerequisites

```bash
# Build and install mux
go install ./cmd/mux

# Start the daemon (uses examples/catalog by default)
mux daemon start
```

## Running the demo

```bash
bash examples/demos/multiplexor/run.sh
# or with a custom catalog:
bash examples/demos/multiplexor/run.sh --catalog ~/.agent-mux/catalog
```

## Expected output

```
=== Agent Mux Multiplexor Demo ===
Catalog: /path/to/examples/catalog

1. Creating session group...
   Group ID: <uuid>

2. Creating sessions...
   Primary:  <uuid>
   Sibling1: <uuid>
   Sibling2: <uuid>

3. Adding sessions to group...
   Added <uuid>
   Added <uuid>
   Added <uuid>

4. Launching sessions...
   Launched <uuid>
   Launched <uuid>
   Launched <uuid>

5. Primary sends request to sibling1 (async mode)...
   Request ID:      <uuid>
   Correlation ID:  <uuid>

6. Sibling1 replies with response...
   Response ID: <uuid>

7. Primary sends notice to sibling2...
   Notice sent

8. Verifying group sessions...
   Sessions in group: 3 (expected 3)

9. Querying correlation thread (workflow_id + correlation_id)...
   Envelopes in thread: 2 (expected 2: request + response)

10. Stopping sessions...
    Stopped <uuid>
    Stopped <uuid>
    Stopped <uuid>

=== Demo complete ===
```

## Blocking request/reply

The demo uses async mode (`POST /broker/requests` without `?wait=true`).
For blocking mode — where the requester waits for the response before
continuing — use:

```bash
# POST /broker/requests?wait=true&timeout=30s
# Returns 200 with the response envelope, or 504 on timeout.
curl -sf --unix-socket ~/.agent-mux/run/muxd.sock \
  -X POST "http://unix/broker/requests?wait=true&timeout=30s" \
  -H "Content-Type: application/json" \
  -d '{"sender":"primary-id","recipient":"sibling-id","payload":"..."}'
```

## Key endpoints

| Endpoint | Purpose |
|----------|---------|
| `POST /session-groups` | Create a new session group |
| `GET /session-groups/{id}/sessions` | List sessions in a group |
| `POST /session-groups/{id}/members` | Add a session to a group |
| `POST /broker/requests` | Post a request (async or blocking) |
| `POST /broker/envelopes` | Post any envelope (response/notice/etc.) |
| `GET /broker/envelopes?workflow_id=…&correlation_id=…` | Query a thread |

## Message types

| Type | When to use |
|------|-------------|
| `request` | Initiating a task; server assigns `correlation_id` |
| `response` | Replying to a request; must carry the request's `correlation_id` |
| `notice` | One-way informational (no reply expected) |
| `status_update` | Periodic progress report |
| `escalation` | Human attention needed |
| `handoff` | Cross-agent session transfer |
