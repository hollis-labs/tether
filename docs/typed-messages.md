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
  `@builder-1`), else of its catalog id. Until then the catalog id (substrate
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

A typed message waits on its recipient when it is unread, the recipient has not
replied to it, and either `awaiting` is the recipient or (with no `awaiting`) it
is a `question`, `decision` or `approval_request`. Replying with `in_reply_to`
or marking it read settles it. The caller's address is matched in its
canonical form; a seat known by two URNs (an alias and its `agt_` id) is matched
only by the one the mail was addressed to. `GET /messages/pending-replies?as=` and
`tether_pending_replies` list them; the Stop gate asks the same question.

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
