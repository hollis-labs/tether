# Typed messages, @handles and seat status

Anything a seat addresses to the system, another seat or Chrispian goes through a
typed envelope (CW-20261114-0013, design v2 sections 5 and 8). The server
validates it and refuses a malformed one with the field and the reason; it never
parses `@handles` out of free text. Tachyon renders each kind as a card.

## Envelope v1

Stored as the message payload with content type
`application/vnd.tether.envelope.v1+json`. Readers that do not know envelopes
still see an ordinary message of the mapped transport kind.

```json
{
  "schema": "tether.envelope/v1",
  "kind": "decision",
  "from": "@photon-tether",
  "to": ["@archie"],
  "re": "task:CW-20261114-0013",
  "attention": "agent",
  "awaiting": "@archie",
  "intent": "pr-split",
  "subject": "Stop gate as a second PR?",
  "body": "The gate plants a hook on every worker launch...",
  "options": [{"id": "one", "label": "One PR"}, {"id": "two", "label": "Two PRs", "recommended": true}],
  "default_option": "two"
}
```

| Field | Rule |
| --- | --- |
| `schema` | Optional on input; stored as `tether.envelope/v1`. |
| `kind` | `question`, `update`, `handoff`, `escalation`, `approval_request`, `decision` or `message`. |
| `from` | Stamped by the server from the caller's verified identity; refused if sent. |
| `to` | 1 to 10 `@handles` (`@[a-z][a-z0-9_-]{0,63}`), no repeats, each must resolve. |
| `re` | Optional: `task:<task id>`, `thread:<id>` or `topic:<slug>`. |
| `in_reply_to` | Optional message id; must be a message you sent or received. The reply joins its thread. |
| `attention` | `human`, `agent` or `none`. Default: `human` when `@chrispian` is a recipient or awaited, else `agent`. |
| `awaiting` | Optional `@handle` that must act next; must resolve. |
| `intent` | Optional tag, `[a-z0-9._-]{1,40}`. |
| `subject` | Optional card title, one line, at most 120 characters. |
| `body` | Required markdown, at most 4000 characters. |
| `options` | `question` and `decision`: 2 to 6. `approval_request`: optional; without options the answers are `approve` and `deny`. Other kinds: none. Ids `[a-z0-9_-]{1,20}`, unique; at most one `recommended`. |
| `default_option` | Optional option id: what happens if nobody answers. |
| `answer` | Only on a reply to a `question`, `decision` or `approval_request`: `{option, note?}`, `option` one of the parent's ids. |

Unknown fields are refused. Transport kind mapping: `question`,
`approval_request`, `decision` are `request`; `update` is `status_update`;
`handoff` and `escalation` keep their names; `message` is `notice`.

Threads are not access-controlled: `re: thread:<id>` places a message in any
thread id the sender names. A send to several recipients stores one message per
recipient and is not atomic; on a 500 the error lists what was sent, and a retry
with the same idempotency key resends only the rest.

Errors come back as HTTP 400 `invalid_request` with
`details: {field, reason}`, for example `envelope.to[1]: unknown handle @unknown`.

## @handles

- `@chrispian` is reserved for the human operator (`msg://user/local/operator`)
  on every path: catalog validation refuses it as a `handle:` (and an agent
  whose id is `chrispian` must set another handle), bootstrap never seeds it,
  and `tether_handle_set` refuses it.
- A seat's handle is its registry external id under substrate `handle`, unique
  among agents. Registry bootstrap gives every catalog agent the handle of its
  catalog `handle:` key when set (`cmd-builder-1` with `handle: builder-1` is
  `@builder-1`), else of its catalog id. A catalog agent with `urn:` keeps that
  registry row: bootstrap attaches its ids there and never mints a second one
  (an `urn:` that is not registered is reported, not created). Until then the catalog id (substrate
  `tether`) and the catalog itself resolve. Catalog validation refuses a
  `handle:` another agent holds as its handle or its id.
- A seat that is not a catalog agent (a local agent-boot session) gets a handle
  from the operator: `tether_handle_set {urn, handle}` / `POST /registry/handles`.
  A handle another agent holds is refused (409).

## Urgency

Urgency is message metadata, not an envelope field: `tether_message_post`
takes `urgency` (`very-low`, `low`, `normal`, `high`; default `normal`) and
`POST /messages/post?urgency=`. The `message.*` lifecycle events carry it as
`urgency`. A critical approval is an `approval_request` at `high`.

## Waiting on a reply

A typed message waits on its recipient when the recipient has not replied to it
(`in_reply_to`), it is not canceled or archived, and either `awaiting` is the recipient or (with no `awaiting`) it is a `question`,
`decision` or `approval_request`. Reading it does not settle it: only a reply
does (settle rule agreed 2026-11-14). Age does not settle it either; each
listed item carries `created_at`. Plain updates and messages never wait.
The caller's address is matched in its canonical form; a seat known by two URNs
(an alias and its `agt_` id) is matched only by the one the mail was addressed
to. `GET /messages/pending-replies?as=` and `tether_pending_replies` list them.

## Stop gate

On worker-hosted Claude sessions with the gate on, the worker plants a Claude
`Stop` hook (`tether-worker hook stop-gate`). When a turn is about to end it asks
the hub `tether_pending_replies` over `/mcp` with the session's agent token. If
a message waits that is younger than 24 hours and has not blocked a stop
before, it blocks this stop with a reason naming those messages and
`tether_message_post`; Claude continues the turn. Each message blocks at most
one stop: the ids it blocked on are kept in `stop-gate.state` in the session
workspace (newest 500), and a state it cannot write means no block. Older
messages still show in `tether_pending_replies`; the watchdog
(CW-20261114-0015) owns them, not the gate. A stop that follows the gate's own
block (`stop_hook_active`) is never gated. It fails open: an unreachable hub, a
timeout (8 s), any error or a panic lets the turn end, with one line on the
hook's stderr and exit 0.

Each block is reported to the hub (`tether_stop_gate_report` /
`POST /messages/pending-replies/blocked`), which publishes `seat.gate_blocked`
`{agent_urn, handle, message_ids, count}` on the seat's session. The report is
best effort: if it fails, the gate still blocks. To inspect a seat by hand:
`stop-gate.state` (ids that blocked), `stop-gate.disabled` (why it was not
planted) and `agent-hooks.json` (the planted hooks) are in the session's
workspace directory on the worker host.

Turning it on: `catalog.defaults.stop_gate` (fleet default, `false`) and an
agent's `stop_gate: true|false` (overrides the default either way). The hub
sends it only to workers that advertise `stop-gate`; an older worker launches
without it. If the session cannot reach the hub's `/mcp` (no agent token or
hub MCP url), the worker launches without the gate and writes
`stop-gate.disabled` with the reason in the session workspace.

## Seat status

`tether_status_set` / `POST /status` declares a seat's state:
`available`, `working`, `waiting-decision`, `waiting-agent`, `blocked`, `paused`
or `offline`, with `reason` (one line, at most 200 characters), `ref`
(`task:<id>` or an `https://` URL), `owner` (an `@handle`, required for
`blocked`) and an expiry (`ttl_seconds` or `expires_at`, at most 7 days). A
seat sets its own; the operator or a catalog `manager` may set another's with
`target`. Re-setting the same state, reason, ref and owner refreshes the expiry
and emits nothing; a change emits `seat.status`
`{agent_urn, handle, state, reason?, ref?, owner?, expires_at?, set_by}`.

`tether_status_get` / `GET /status/{@handle}` returns the card:

```json
{
  "handle": "@photon-tether",
  "agent_urn": "msg://agent/agent-mux/agt_lpy0gym4eh",
  "declared": {"state": "blocked", "reason": "waiting on the harness tag", "owner": "@archie", "since": "...", "expires_at": "...", "set_by": "..."},
  "effective": "blocked",
  "live": {"session_id": "...", "online": true, "health": "idle", "work_state": "idle", "last_activity": "...", "unread_mail": 0, "pending_replies": 1}
}
```

`effective` is the declared state, `unknown` when it expired or was never
declared, and `undeclared` when the seat is online without a declaration.

## Events

- `message.*` lifecycle events of a typed message carry `envelope_kind`,
  `attention`, `awaiting`, `intent` and `re` (never the subject, body or options).
- `seat.status` as above.
