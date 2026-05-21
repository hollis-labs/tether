# Messaging

Tether's messaging surface is the durable mailbox for agents, users, services,
sessions, and workflows. It stores envelopes in the local state database,
supports pull and subscribe delivery, and can now wake a live session after
mail is written.

## Addressing

Messages use canonical `msg://` URNs:

```text
msg://agent/<authority>/<logical_agent_id>
msg://session/<authority>/<session_id>
msg://user/<authority>/<user_id>
msg://service/<authority>/<service_id>
msg://workflow/<authority>/<workflow_id>
```

Examples:

```text
msg://agent/agent-mux/torque-supervisor
msg://session/local/e672176e-9f16-49de-a922-5d9c35ca6bd1
msg://user/local/operator
```

`<authority>` is the routing owner. In standalone mode every authority is
treated as local. With federation enabled, known peer authorities route to
their configured daemon; see [Messaging federation](messaging-federation.md).

## Message Kinds

Kinds describe what the message is:

| Kind | Use |
|---|---|
| `request` | A peer is expected to respond. |
| `response` | Reply to a prior `request`. |
| `notice` | One-way informational mail. |
| `status_update` | Progress, health, pass logs, state changes. |
| `handoff` | Transfer context or ownership to another agent. |
| `escalation` | Needs elevated attention or operator handling. |

Urgency is separate from kind. Use metadata key `urgency` with the RFC 8030
Web Push vocabulary:

```text
very-low | low | normal | high
```

For example, prefer `kind=notice, urgency=high` over inventing an `alert`
kind. Prefer `kind=escalation, urgency=high` when the message itself is an
escalation.

## Delivery Modes

### Store Only

`POST /messages`, MCP `mux_message_send`, and CLI `mux messages send` persist a
message. They do not inject anything into a running agent session.

```sh
mux messages send \
  --from msg://user/local/operator \
  --to msg://agent/agent-mux/torque-supervisor \
  --kind notice \
  --subject "Scope clarification" \
  "Round 2 scope clarification is ready."
```

### Notify And Wake

`POST /messages/notify`, MCP `mux_message_notify`, and CLI
`mux messages notify` persist the same durable message and then best-effort
wake a live session with a daemon-injected mailbox reminder turn.

```sh
mux messages notify \
  --from msg://user/local/operator \
  --to msg://agent/agent-mux/torque-supervisor \
  --urgency high \
  --subject "Mailbox wake" \
  "Please check your inbox and process pending messages."
```

Wake resolution:

| Recipient / option | Resolution |
|---|---|
| `--session <session-id>` / `session_id` | Wake that explicit live session. |
| `msg://session/<authority>/<session_id>` | Wake that live session. |
| `msg://agent/<authority>/<logical_agent_id>` | Wake the newest running session for that logical agent when found. |
| Offline or unresolved recipient | Message is stored; wake fields report no delivery. |

Notify response fields:

| Field | Meaning |
|---|---|
| `message` | Stored envelope. |
| `unread_count` | Current unread count for the recipient after storing. |
| `wake_attempted` | A live session was resolved and the daemon tried to wake it. |
| `wake_delivered` | Wake turn was accepted by the session runtime. |
| `wake_error` | In-band wake failure; the durable message still exists. |

The default wake text tells the agent it has unread mail and asks it to run its
normal inbox-check procedure. It does not require the agent to abandon current
work unless the agent's own checklist treats the message as urgent.

Current limitation: notify is best-effort for currently live sessions. Tether
does not yet run a background retry worker that wakes an agent later when it is
resumed or relaunched.

## Reading Mail

There are two read surfaces with different semantics.

| Surface | Behavior | Use |
|---|---|---|
| `inbox` | Destructive agent pull. Returned messages are marked `delivered_at` and will not appear in later inbox pulls. | Agent runtime checklists. |
| `list` | Non-destructive repeatable listing with `read_at`, `archived_at`, and `canceled_at` state. | Operator UIs, dashboards, audits. |

CLI examples:

```sh
# Agent-style pull; marks returned messages delivered.
mux messages inbox msg://agent/agent-mux/torque-supervisor

# UI/operator-style read; does not mark delivered.
mux messages list msg://agent/agent-mux/torque-supervisor --unread-only

# Mark handled mail.
mux messages read <message-id> --as msg://agent/agent-mux/torque-supervisor
mux messages consume <message-id> --as msg://agent/agent-mux/torque-supervisor
mux messages archive <message-id> --as msg://agent/agent-mux/torque-supervisor
```

## Subscriptions

`GET /messages/subscribe?to=<urn>` streams newly-created messages for a
recipient. It does not replay historical mail; use `inbox` or `list` for
history. A connected UI or keeper process can subscribe and react immediately,
but durable delivery still lives in the message store.

## Group Mentions

Group `@` mentions emit `notice` envelopes to mentioned agents' personal
inboxes. See [Group symbols](groups/symbols.md) for group-specific mention,
command, and directive conventions.

## Surfaces

| Surface | Commands / routes |
|---|---|
| CLI | `mux messages send`, `notify`, `get`, `inbox`, `list`, `thread`, `read`, `consume`, `archive`, `unarchive`, `cancel` |
| MCP | `mux_message_send`, `mux_message_notify`, `mux_message_get`, `mux_message_inbox`, `mux_message_list`, `mux_message_thread`, `mux_message_mark_read`, `mux_message_consume`, `mux_message_archive`, `mux_message_unarchive`, `mux_message_cancel` |
| HTTP | `/messages`, `/messages/notify`, `/messages/{id}`, `/messages/inbox`, `/messages/list`, `/messages/thread/{thread_id}`, `/messages/subscribe` |
