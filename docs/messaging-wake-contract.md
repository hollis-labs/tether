# Message delivery and wake contract

What Tether promises when a message is sent to a seat, and how a caller (for
example the lead watchdog, CW-20261118-0023) tells "stored", "woken" and
"handled" apart. Checked against the code on `main` and against live traces on
2026-11-18 (CW-20261118-0024). Related: [messaging.md](messaging.md),
[mail-triage.md](mail-triage.md), [typed-messages.md](typed-messages.md).

## The five stages

| # | Stage | Meaning | Where it shows |
|---|---|---|---|
| 1 | **stored** | The message row and its delivery obligation are committed. The send call succeeded. | The send returns the message `id`; trace receipt `persisted`. |
| 2 | **wake attempted** | A live session was resolved, the delivery claimed for it, and a turn handed to the runtime. | `wake_outcome.attempted=true`; event `wake.attempted`; a trace attempt with `holder_kind: session`. |
| 3 | **delivered** | The runtime accepted the wake turn. The delivery stays leased at `turn_submitted` for up to 15 min (`wakeConsumeWindow`) awaiting the recipient. | `wake_outcome.delivered=true`; trace attempt stage `turn_submitted`, `holder_kind: session`. |
| 4 | **agent processing** | The woken session is running a turn. Tether does not record this per message. | Session state `processing` (`tether_session_list` / runtime health). Not a message field. |
| 5 | **read / consumed** | The recipient acted: `mark_read`, `consume`, or its own session's inbox pull. This settles the delivery. | Trace `read_at` / `consumed_at`; delivery status `delivered`. |

Rules that follow:

- **Delivered is not handled.** `delivered=true` means a turn reached the
  session. It says nothing about whether the agent read, understood or did the
  work. Only stage 5 says the mail was handled, and even that is not completion
  of the work it asked for.
- **Stage 5 can happen without stages 2 and 3.** An offline seat that later
  pulls or reads its mail settles the delivery with no wake.
- **Trace stages are delivery-core receipts, not wake proof.** `mark_read` and
  `consume` record `host_accepted`, `turn_submitted` and `consumed` together,
  in one millisecond, on an attempt held by the recipient address. Such an
  attempt has `holder_kind: recipient`. Read it as "the recipient settled the
  delivery", never as "a turn was submitted". A push wake's attempt is held by
  a session id (`holder_kind: session`).
- **Delivery status `delivered`** in the trace is delivery-core's terminal
  state, reached only by stage 5 (or by the wake path finding the message
  already read). A wake alone leaves the delivery `leased`; if nobody reads it
  within the consume window the lease lapses into one retry, which re-wakes the
  seat as a reminder.

## The wake outcome: `{attempted, delivered, reason}`

Every wake decision produces a `WakeOutcome` (`internal/api/types.go`):

| Field | Meaning |
|---|---|
| `attempted` | A turn was handed to a session runtime (`SendTurn` was called). False for every reason below that stops earlier. |
| `delivered` | The runtime accepted the turn. Implies `attempted`. |
| `session_id` | The session the turn went to (or was meant for). |
| `reason` | Empty on delivery. Otherwise one fixed label (below). |
| `detail` | Free text for `turn-submit-failed` and similar; never in events. |

Reasons, grouped by what they mean for the message:

- **Not woken by policy, mail stays unread:** `no-wake` (sender asked),
  `triaged-fyi`, `triaged-read`, `triaged-covered`, `deferred-cap` (triage; a
  deferred action is woken by the sweep after the seat's interval), `pull-only`
  (a published-local bridge must poll).
- **No session to wake, retried:** `offline`, `recipient-unavailable`. The
  sweep retries when a session for the recipient comes online (parked with
  backoff, 30 s to 2 min).
- **Session found but not woken now, retried:** `busy` (retried at the
  session's turn end, or by backoff up to 60 s; never given up on),
  `offline-race`, `stale-generation`, `claim-unavailable`,
  `marker-write-failed`, `settle-failed`.
- **Attempted and failed, retried:** `turn-submit-failed`.
- **Already settled:** `already-handled` (the recipient read or consumed it
  before the wake ran).
- **Unknown:** `wake-outcome-unknown` (a crash after admission; never resent,
  because the turn may have gone out), `invalid-wake-intent`,
  `untracked-delivery`, `wake-admission-failed`, `binding-check-failed`.

Where it appears:

- `tether_message_send` (`POST /messages`): the returned envelope carries it
  as `metadata["tether.wake_outcome"]` (a JSON string). It is a response-only
  observation: it is never stored on the message, so `tether_message_get` does
  not show it, and it never changes the message digest.
- `tether_message_notify` (`POST /messages/notify`): flattened into
  `wake_attempted`, `wake_delivered`, `wake_reason`, `wake_error`.
- `tether_message_post` / `tether_ask` / `tether_reply`
  (`POST /messages/post`): return only `ids`. Read the outcome from the trace.
- `tether_message_trace`: `wake_outcome` is the recorded outcome of the
  admitted wake, updated by each sweep retry. It is absent when no wake was
  admitted (the first decision stopped before a session was resolved:
  offline, triaged, no-wake). Every decision, admitted or not, is also an
  event: `wake.outcome` (and `wake.attempted` just before a turn is
  submitted), with `message_id`, `target`, `attempted`, `delivered`, `reason`,
  and `attempt_id`/`binding_generation` when a claim was made.

## Attention and awaiting

Tether interprets `attention` and `awaiting` **only on typed envelopes**
(`content_type: application/vnd.tether.envelope.v1+json`, sent with
`tether_message_post`, `tether_ask` or `tether_reply`). In an untyped message
(`tether_message_send` with `payload_json`) they are opaque payload fields:
the server never reads them, and the wake treats the message by its transport
kind.

- **`attention`** is `human`, `agent` or `none`. It matters only for a seat
  with `mail_triage` enabled (per agent in the catalog; off by default). For a
  seat without triage, **every** message that resolves a live session wakes
  it, whatever its attention. With triage on, the order is human > action >
  read > fyi ([mail-triage.md](mail-triage.md)):
  - `attention: human` (or the default when `@chrispian` is a recipient or
    awaited) wakes now, never capped.
  - `attention: none` makes a typed `update`/`message` class `read`: never
    woken, marked read by the hub at once.
  - `attention: none` **does not** override an action rule. A typed
    `attention: none` reply to mail the seat sent is `reply_to_own` and wakes
    it (subject to the action cap). So does `urgency: high`, `awaiting` the
    seat, or an ask kind (`question`, `decision`, `approval_request`,
    `request`, `handoff`, `escalation`). This is the deliberate design of
    CW-20261117-0002 and is pinned by `triage_test.go`.
- **`awaiting`** on a typed envelope is the `@handle` that must act next; it
  makes the message `action` for that seat. On an **untyped reply**
  (`in_reply_to` set), the server stamps `metadata.awaiting` with the parent
  message's sender when the caller did not set it (CW-20261117-0003), so every
  reply names the asker. A payload `"awaiting": null` in untyped mail does not
  suppress that: the server does not read the payload. To say "nobody is
  awaited", send a typed envelope with no `awaiting`.

## Idempotency and duplicate sends

- Pass `idempotency_key` (MCP) or `Idempotency-Key` (HTTP). The key is scoped
  to the sender and the verified principal; a typed post to several recipients
  gets `<key>:<n>` per recipient.
- A repeat with the same key and the same content returns the **same message
  id** and stores nothing. Different content under the same key is refused
  with `idempotency_conflict` (HTTP 409).
- A message is woken **at most once at send time**: the first wake to resolve
  a live session is admitted (`recipient_wake_admissions`), and only the sweep
  retries after that. A duplicate send of an admitted message reports the
  stored outcome (the first send's, or the latest sweep retry's) and never
  submits a second turn. A message never admitted (its first send found the
  seat offline or triaged it) is evaluated afresh on a duplicate, which may
  wake it if the seat has come online.
- Without a key, MCP sends use a fresh key per call (retried only within that
  call), so two calls are two messages and two wakes.

## mark_read, consume, inbox, list

| Call | Side effects | Settles the delivery (stops wake retries) |
|---|---|---|
| `tether_message_list` | None. Repeatable. | No |
| `tether_message_get` | None. | No |
| `tether_message_mark_read` | Sets `read_at`. | Yes |
| `tether_message_consume` | Sets `consumed_at`. | Yes |
| `tether_message_inbox` | **Takes the batch**: stamps `delivered_at` on every message returned, which then never comes back from `inbox`. Consumes each one only when the caller is the recipient's own bound session. | Only for the recipient's own session |

An agent reading its mail should `list` (unread only) and `mark_read` each
message it handled. `inbox` is for a recipient session pulling its own mail.

## Retries and redrive

- Wake retries are automatic (the sweep, every few seconds): busy, offline,
  failed and lapsed-lease deliveries. Offline and failed attempts back off
  exponentially (cap 300 s); from the 12th attempt the delay jumps to the
  6 h message-age bound, so the wake effectively stops while the mail stays in
  the inbox. Busy is retried indefinitely at the turn end. The sweep does not wake for mail older
  than 6 h on an untriaged seat; it stays in the inbox.
- Reading or consuming the message ends all retries; a retry that finds it
  read settles it (`already-handled`) instead of waking.
- `tether_message_redrive` applies **only to a dead-lettered delivery**.
  On a pending, leased or retry-scheduled delivery it is a no-op
  (`redriven=false`); on a delivered one it is a conflict. It is a repair
  tool, not a way to re-wake a seat. To remind a seat, send a new message.

## Which session a wake goes to

For `msg://agent/<authority>/<id>`:

1. The actor's **current registry binding** (`registry.CurrentBinding`, newest
   generation) names the session. If that session is not running the outcome
   is `offline`: Tether never falls back to another running session of the
   same agent.
2. A pull-only (published-local bridge) binding is never woken (`pull-only`).
3. Only an actor that was never bound, or whose last binding lapsed and was
   not pull-only, falls back to the newest running session with that logical
   agent id.
4. The binding generation is checked at claim and again just before the turn
   is submitted; if it moved (the session was replaced), the attempt stops as
   `stale-generation` and is retried against the new binding. A replaced
   session is never woken for its successor's mail.

`msg://session/<authority>/<id>` wakes exactly that session if it is alive.
Mail to an alias wakes the canonical address's session.

## Idle, busy, offline

| Session state | Outcome | Then |
|---|---|---|
| Idle | `attempted, delivered` | Lease held for the recipient to read it. |
| Busy (`processing`) | `busy`, not attempted | Retried when the turn ends (re-armed), else by backoff. |
| Offline / no session | `offline`, not attempted | Retried when a session binds and comes up. |

## Supported watchdog invocation

The watchdog asks a lead to act; it must never take delivery for completion.

1. **Send** a typed request with `tether_message_post` (HTTP
   `POST /messages/post`):
   - `kind: request`, `to: ["@<lead>"]`, `attention: agent`,
     `re: task:<task id>` when there is one, a short `subject`/`body`;
   - no `awaiting` (the request kind already makes it action);
   - `urgency: normal` (use `high` only for an escalation that must beat the
     action cap);
   - `idempotency_key: watchdog:<lead handle>:<episode id>`, where an episode
     is one continuous idle period. Retrying within an episode with the same
     key returns the same message and cannot double-wake; a new episode gets a
     new key.
   - Record the returned `ids[0]` against the episode.
2. **Read the outcome** with `tether_message_trace(message_id)`:
   - `wake_outcome.delivered=true`: a turn reached the lead's current session
     (`wake_outcome.session_id`). The lead was woken, nothing more.
   - `wake_outcome` absent or `delivered=false`: read `reason` from the
     `wake.outcome` events (`tether_events_history kind=wake.outcome`, match
     `message_id`). `busy`, `offline`, `deferred-cap` are retried by Tether
     itself; do not resend.
   - `read_at` / `consumed_at` set: the lead handled the mail.
   - Ignore `turn_submitted` on an attempt with `holder_kind: recipient`.
3. **Judge completion from the work**, not from mail: the task's state, a
   checkpoint, or the lead's `tether_reply` with `status: done|declined|blocked`.
4. **Escalate** when the episode's message is still unread after the agreed
   window, or the trace shows a terminal reason (`pull-only`, dead-lettered
   delivery). Send the escalation as a new message with its own key; never
   redrive a live delivery to re-wake.

For a lead without `mail_triage`, every send that finds it idle wakes it, so
the watchdog's own cooldown is the only rate limit. With `mail_triage` on, the
seat's `action_interval` (default 3 m) also caps it.
