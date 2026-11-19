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
| `kind` | `request`, `question`, `update`, `handoff`, `escalation`, `approval_request`, `decision` or `message`. `request` is "do X and report back" (CW-20261117-0003). |
| `from` | Stamped by the server from the caller's verified identity; refused if sent. |
| `to` | 1 to 10 `@handles` (`@[a-z][a-z0-9_-]{0,63}`), no repeats, each must resolve. |
| `re` | Optional: `task:<task id>`, `thread:<id>` or `topic:<slug>`. |
| `in_reply_to` | Optional message id; must be a message you sent or received. The reply joins its thread. When `to` or `awaiting` is unset, the server fills it with the parent sender's `@handle`, so the reply reaches and wakes the asker (not for a follow-up to your own message). |
| `attention` | `human`, `agent` or `none`. Stored only when the sender sets it; readers derive the default (`human` when `@chrispian` is a recipient or awaited, else `agent`). |
| `awaiting` | Optional `@handle` that must act next; must resolve. |
| `intent` | Optional tag, `[a-z0-9._-]{1,40}`. |
| `subject` | Card title, one line, at most 120 characters. Required on `request`; on `question`, `decision` and `approval_request` it defaults to the body's first line; optional otherwise. |
| `body` | Required markdown, at most 4000 characters. |
| `options` | `question` and `decision`: 2 to 6. `approval_request`: optional; without options the answers are `approve` and `deny`. Other kinds: none. Ids `[a-z0-9_-]{1,20}`, unique; at most one `recommended`. |
| `default_option` | Optional option id: what happens if nobody answers. |
| `deliverable` | Required on `request` and only there: what done looks like, at most 1000 characters. A `request` takes no options. |
| `status` | Optional on a reply only (`in_reply_to` set): `done`, `declined` or `blocked`. |
| `evidence` | Optional list of up to 10 one-line pointers (PR, commit, path, URL), each at most 500 characters. |
| `answer` | Only on a reply to a `question`, `decision` or `approval_request`: `{option, note?}`, `option` one of the parent's ids. |
| `on_behalf_of` | Optional: who the sender acts for, as a `@handle` (must resolve) or a principal URN. See "On behalf of" below. |
| `drafted_by` | Optional: `assistant` or `human`, only with `on_behalf_of`; stored as `assistant` when omitted. |

Unknown fields are refused. Transport kind mapping: `request`, `question`,
`approval_request`, `decision` are `request`; `update` is `status_update`;
`handoff` and `escalation` keep their names; `message` is `notice`.

Threads are not access-controlled: `re: thread:<id>` places a message in any
thread id the sender names. A send to several recipients stores one message per
recipient and is not atomic; on a 500 the error lists what was sent, and a retry
with the same idempotency key resends only the rest.

Errors come back as HTTP 400 `invalid_request` with
`details: {field, reason}`, for example `envelope.to[1]: unknown handle @unknown`.

## Ask and reply tools

`tether_ask` and `tether_reply` (MCP, message.write) are the way to ask and
answer (CW-20261117-0003). They build the envelope, the daemon validates it
(refusing with the field and reason) and the sender is the caller's identity.

- `tether_ask(kind, to, subject, body, deliverable?, re?, options?, default_option?, attention?, urgency?)`:
  kind `request` (needs `deliverable`), `question`, `decision`, `approval_request`,
  `handoff` or `escalation`. A typed `request` triages as action (`envelope_kind`);
  the recipient's wake shows the sender's handle, subject and deliverable.
- `tether_reply(in_reply_to, body, answer?, note?, status?, evidence?, subject?, to?)`:
  sends kind `message` to the asker (`to` defaults to the parent's sender), with
  `awaiting` set to the asker, so it wakes them (`awaiting_recipient`, and
  `reply_to_own` for seats with mail triage). `answer` must be one of the ask's
  options.

A plain reply (not itself an ask) never holds anyone's Stop gate: the asker is
woken by it but does not owe a reply to the reply. Use Torque checkpoints, not
these tools, for status and progress.

Untyped replies sent with `tether_message_send` and `in_reply_to` get
`metadata.awaiting` set to the parent's sender URN.

## On behalf of

An assistant that sends mail for someone (Basalisk's assistant for Chrispian)
sends it from its own address and says so (CW-20261114-0116):

```json
{"kind": "message", "to": ["@archie"], "body": "Chrispian asks for the deploy notes.",
 "on_behalf_of": "@chrispian", "drafted_by": "assistant"}
```

`on_behalf_of` is the **sender's claim, not an identity**. It never changes
`from` (the server still stamps the sender) or `from_principal`, and it grants
the sender none of the other person's permissions. Sending as someone else is
still refused (see address aliases in `docs/messaging.md`).

- **Triage:** mail with `on_behalf_of` from a sender that is not a `user`
  address is class **`action`**, reason `on_behalf_of`, never `human` and never
  `from_operator`, whatever its `attention` or recipients. It is not Chrispian's
  own instruction. Mail the operator sends himself is unaffected (`from_operator`),
  as is mail from a `user` sender.
- **Events:** `mail.triaged` and the `message.*` lifecycle events carry
  `on_behalf_of` and `drafted_by`.
- **Wakes:** the mailbox-wake text says "Sent on behalf of `@chrispian`
  (drafted by assistant): it is the sender's, not @chrispian's own instruction."
- **MCP:** `tether_message_post` takes `on_behalf_of` and `drafted_by`.

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

## Operator-attested replies

Chrispian can answer a `question`, `decision` or `approval_request` addressed to
him from Tachyon, and Tether stores it as **his verified reply**: `from` is his
canonical address, `from_principal` is `msg://user/local/operator`,
`from_verification` is `verified`, and `operator_attestation` records the proof.
The reply settles the parent in `GET /messages/pending-replies` and triages
`from_operator`. `on_behalf_of` never does this. The full contract is
[operator-attested-replies.md](operator-attested-replies.md) (CW-20261114-0152).

- **Off by default.** The feature needs an `operator_attestation` block in
  global.yaml: Tangent's issuer, `max_age_seconds` (at most 300) and one or two
  Ed25519 keys. The block only takes effect through an operator-approved
  catalog plan; a hand-edited block is refused (audited
  `operator_attestation_refused`). Without it every route answers 404
  `feature_disabled`.
- **Scopes.** `message.operator_reply` (Tachyon) and `message.operator_reply_view`
  (Tangent) are device scopes given at pairing. They work only for a verified
  device the operator paired, never for an issued token; anything else is 403
  `not_operator_device` or `scope_required`.
- **Routes.**
  - `POST /messages/{id}/operator-reply/challenge` `{"option"}` returns a
    single-use challenge (`challenge_id`, `nonce`, `binding`, `expires_at`);
    at most 10 open per device.
  - `GET /operator-attestation/challenges/{challenge_id}` (and `.../body`) is
    the card Tangent shows and signs: kind, the asker with its
    `from_verification`, subject, the chosen option, a body excerpt and the
    full body.
  - `POST /messages/{id}/operator-reply`
    `{"challenge_id","option","note"?,"assertion"}` stores the reply. The
    assertion is Tangent's EdDSA JWS (`typ: operator-reply+jwt`) bound to the
    challenge, the option, the body and the note.
- **Freshness** is Tether's own: the assertion and the step-up must be at most
  `max_age_seconds` old (30 s skew), whatever the operator's `elevation_ttl`
  says.
- **One use.** A challenge is consumed by the first reply attempt naming it,
  pass or fail; a `jti` or `elevation_id` is spent once; a card is answered
  once.
- **Failure codes:** `feature_disabled`, `not_operator_device`,
  `scope_required`, `parent_not_found`, `parent_not_operators`,
  `not_answerable`, `invalid_option` (both with `{field, reason}`),
  `already_answered`, `challenge_unknown`, `challenge_expired`,
  `assertion_invalid`, `assertion_stale`, `assertion_unbound`, `replayed`,
  `too_many_challenges`. Nothing is stored on a failure.
- **Reserved metadata.** `operator_attestation` and every key under that
  prefix (any case), and the sender stamp (`from_verification`,
  `from_unverified`, `from_principal`), are set only by the server. Every
  ingress drops a client's value (`POST /messages`, `/messages/notify`,
  `/messages/request` and `/messages/post`, and so the MCP tools and federation
  peers that use them), and the message store drops `operator_attestation` from
  any send but the attested-reply path, so a stored value is always genuine.
- **Audit.** Every attempt is an `identity_audit` row, `operator_reply_attested`
  or `operator_reply_refused`, with the device, the parent, the challenge, the
  elevation, the refusal code and the parent sender's `from_verification`.

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
`stop-gate.disabled` with the reason in the session workspace. A
group-writable (0775) release directory is another such reason; see "Hook
binary permissions" in `docs/worker-service.md`.

## Seat status

`tether_status_set` / `POST /status` declares a seat's state:
`available`, `working`, `waiting-decision`, `waiting-agent`, `blocked`, `paused`
or `offline`, with `reason` (one line, at most 200 characters), `ref`
(`task:<id>` or an `https://` URL), `owner` (an `@handle`, required for
`blocked`) and an expiry (`ttl_seconds` or `expires_at`, at most 7 days). A
seat sets its own; the operator or a catalog `manager` may set another's with
`target`. Re-setting the same state, reason, ref and owner refreshes the expiry
and emits nothing; a change emits `seat.status`
`{agent_urn, handle, state, reason?, ref?, owner?, expires_at?, set_by,
set_by_verification?, set_by_principal?}`.

Who may set it (CW-20261114-0130): the setter's `?as=` claim must hold, in every
identity mode. A claim the hub cannot verify (an `?as=` the authenticated
principal does not own) sets nothing (403), even in observe mode, where other
routes only record it. The seat itself (verified, or stamped from the caller's
own address), a verified catalog manager, and the operator credential may set
it; without a verified principal only identity-off on the local socket may.
Every declaration records `set_by_verification` (`verified`, `stamped`, or empty
with no principal) and `set_by_principal`, the way mail records
`from_verification` and `from_principal`.

`tether_status_get` / `GET /status/{@handle}` returns the card:

```json
{
  "handle": "@photon-tether",
  "agent_urn": "msg://agent/agent-mux/agt_lpy0gym4eh",
  "declared": {"state": "blocked", "reason": "waiting on the harness tag", "owner": "@archie", "since": "...", "expires_at": "...", "set_by": "...", "set_by_verification": "verified", "set_by_principal": "msg://session/local/..."},
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
