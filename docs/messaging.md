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

`POST /messages`, MCP `tether_message_send`, and CLI `tether messages send` persist a
message. They do not inject anything into a running agent session.

```sh
tether messages send \
  --from msg://user/local/operator \
  --to msg://agent/agent-mux/torque-supervisor \
  --kind notice \
  --subject "Scope clarification" \
  "Round 2 scope clarification is ready."
```

### Notify And Wake

`POST /messages/notify`, MCP `tether_message_notify`, and CLI
`tether messages notify` persist the same durable message and then best-effort
wake a live session with a daemon-injected mailbox reminder turn.

```sh
tether messages notify \
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
| `wake_reason` | Observational, non-error disposition when the wake did not land: `busy`, `offline-race`, `stale-generation`, `claim-unavailable`, `marker-write-failed`. The delivery was released for retry, not lost. |
| `wake_error` | In-band wake failure; the durable message still exists. |

The default wake text tells the agent it has unread mail and asks it to run its
normal inbox-check procedure. It does not require the agent to abandon current
work unless the agent's own checklist treats the message as urgent.

### Retry

A notify call's own wake attempt is synchronous and best-effort, but a wake that
cannot land immediately is **not lost**. A busy session (`wake_reason: busy`) or
an unresolved recipient releases the delivery for retry with a bounded backoff,
and the daemon runs a background wake sweep on a short ticker that retries ready
deliveries through the exact same path a fresh notify uses, re-resolving the
recipient each pass. In practice a wake sent while an agent is mid-turn lands as
a new turn shortly after that turn finishes.

What Tether still does not do is **relaunch** anything to deliver mail. It never
assumes launch or resume authority over a stopped session. Mail for an actor
with no live session accumulates durably and is delivered once a session owns
its binding again — started by you, not by the daemon.

## Reading Mail

There are two read surfaces with different semantics.

| Surface | Behavior | Use |
|---|---|---|
| `inbox` | Destructive agent pull. Returned messages are marked `delivered_at` and will not appear in later inbox pulls. | Agent runtime checklists. |
| `list` | Non-destructive repeatable listing with `read_at`, `archived_at`, and `canceled_at` state. | Operator UIs, dashboards, audits. |

CLI examples:

```sh
# Agent-style pull; marks returned messages delivered.
tether messages inbox msg://agent/agent-mux/torque-supervisor

# UI/operator-style read; does not mark delivered.
tether messages list msg://agent/agent-mux/torque-supervisor --unread-only

# Mark handled mail.
tether messages read <message-id> --as msg://agent/agent-mux/torque-supervisor
tether messages consume <message-id> --as msg://agent/agent-mux/torque-supervisor
tether messages archive <message-id> --as msg://agent/agent-mux/torque-supervisor
```

## Sysop local user and From default

Sysop's user inbox aggregates all user addresses before pagination. Read and
archive filters persist through refresh; the optional Recipient filter accepts
a canonical address or a configured readable alias. Opening an unread message
keeps its detail and reply controls available after it leaves the unread page.

The temporary local user record is `user-profile.json` in Tether's XDG data
directory (`$XDG_DATA_HOME/tether`, normally `~/.local/share/tether`). For example:

```json
{
  "urn": "msg://user/local/chris",
  "aliases": [
    {"urn": "msg://user/local/chris", "alias": "chris"},
    {"urn": "msg://user/agent-mux/chris", "alias": "chris-mux"}
  ],
  "messaging": {"from_default": "msg://user/agent-mux/chris"}
}
```

With no file, the local user is `msg://user/agent-mux/operator` and no preference
is saved. With no saved default, compose uses the configured local user URN.
Use **Save From default** in New Message to persist a canonical address or
resolved alias; clear From and save to remove the preference. An invalid saved
value surfaces an error instead of selecting another sender. Fix the local
record to recover from an invalid saved value.

Sysop exposes `GET /api/messages/profile` and
`POST /api/messages/profile` with only `{"from_default":"<address-or-alias>"}`
(empty string clears it). The server fixes the identity and preference key;
clients cannot choose either. Saves atomically replace an owner-only record.
This record supplies local preferences, not login or authentication. The real
user model is tracked separately by CW-20261008-0137.

Aliases in the local profile are case-insensitive, one readable alias per user
address. Sysop combines them with the existing message-alias directory and
refuses ambiguous names. Manage readable aliases updates directory entries;
edit the local record for aliases it owns. Address and alias changes never
rewrite stored envelopes: sending resolves to canonical URNs.

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

### Core send/read

| Surface | Commands / routes |
|---|---|
| CLI | `tether messages send`, `notify`, `get`, `inbox`, `list`, `thread`, `read`, `consume`, `archive`, `unarchive`, `cancel` |
| MCP | `tether_message_send`, `tether_message_notify`, `tether_message_get`, `tether_message_inbox`, `tether_message_list`, `tether_message_thread`, `tether_message_mark_read`, `tether_message_consume`, `tether_message_archive`, `tether_message_unarchive`, `tether_message_cancel` |
| HTTP | `/messages`, `/messages/notify`, `/messages/{id}`, `/messages/inbox`, `/messages/list`, `/messages/thread/{thread_id}`, `/messages/subscribe` |

### Identity and runtime bindings

Who an actor durably *is*, and which live session currently receives its mail.

| Surface | Commands / routes |
|---|---|
| CLI | `tether registry register`, `update-self`, `deregister`, `lookup`, `search`, `merge`, `sync`, `tether whoami`, `tether registry binding lease\|renew\|revoke\|current\|list` |
| MCP | `tether_whoami`, `tether_registry_register`, `tether_registry_update_self`, `tether_registry_deregister`, `tether_registry_lookup`, `tether_registry_lookup_by`, `tether_registry_search`, `tether_registry_merge`, `tether_registry_sync`, `tether_registry_binding_lease`, `tether_registry_binding_renew`, `tether_registry_binding_revoke`, `tether_registry_binding_current`, `tether_registry_binding_list`, `tether_registry_scoped_binding_set`, `tether_registry_scoped_binding_resolve`, `tether_registry_scoped_binding_revisions` |
| HTTP | `/whoami`, `/registry`, `/registry/bindings`, `/registry/scoped-bindings`, `/registry/scoped-bindings/resolve`, `/registry/scoped-bindings/revisions`, `/sessions/bootstrap` |

### Groups

| Surface | Commands / routes |
|---|---|
| MCP | `tether_group_create`, `tether_group_post`, `tether_group_read`, `tether_group_lookup`, `tether_group_invite`, `tether_group_kick`, `tether_group_leave`, `tether_group_archive`, `tether_group_set_role`, `tether_group_mark_read`, `tether_group_mentions`, `tether_group_list_members`, `tether_group_list_for_member` |
| HTTP | `/groups`, `/session-groups` |

### Delivery trace, repair and retention

| Surface | Commands / routes |
|---|---|
| MCP | `tether_message_trace`, `tether_message_redrive`, `tether_message_purge`, `tether_message_retention_candidates` |
| HTTP | `/messages/{id}/trace`, `/messages/{id}/redrive`, `/messages/{id}/purge`, `/messages/retention/candidates`, `/messages/{id}/claim`, `/messages/{id}/ack`, `/messages/{id}/nack` |

`claim`/`ack`/`nack` are the durable-delivery primitives. They are raw HTTP
only — `go-tether-client` has no typed wrapper for them yet (CW-20260907-0038).

> **Every messaging read must assert a caller identity** via `?as=<urn>`
> (ADR 0045). See [messaging-adoption.md](./messaging-adoption.md).
