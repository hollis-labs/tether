# Hub mail triage before wake

Mail is a record, not a turn (CW-20261114-0017, design v2 section 5). For a seat
with mail triage on, the hub classifies each message once before it may wake
the seat, and only actionable mail costs a turn.

## Turning it on

```yaml
# global.yaml: catalog.defaults (fleet default, off)
mail_triage:
  enabled: false
  action_interval: 3m

# agents/<id>.yaml (overrides the default field by field)
mail_triage:
  enabled: true
  action_interval: 2m
```

Only mail to a catalog agent's address (its `urn:` or
`msg://agent/agent-mux/<id>`) is triaged; a seat without triage is woken
exactly as before. A catalog reload applies a change to mail stored from then
on.

## Classes

Shared order **human > action > read > fyi** (the same as the local tether-mail
triage): the first class with a matching rule wins, so `attention: none` never
overrides an action rule.

| Order | Class | Rules | What happens |
| --- | --- | --- | --- |
| 1 | `human` | from the operator; envelope attention human (set by the sender, or the default when `@chrispian` is a recipient or awaited) | wakes now, never capped |
| 2 | `action` | `urgency: high`; a reply to mail this seat sent (`reply_to_own`: its `in_reply_to` names a message from the seat, by urn or catalog-id form); envelope `awaiting` this seat; envelope `question`, `decision`, `approval_request`, `escalation`, `handoff`; untyped `request`, `escalation`, `handoff` | wakes, at most once per `action_interval` per seat |
| 3 | `read` | envelope `attention: none` set by the sender (marked read by the hub, `message.read`); untyped `urgency: very-low` (left unread) | no wake |
| 4 | `fyi` | envelope `update`, `message`; untyped `notice`, `status_update`, `response` | no wake; rides along on the seat's next wake (digest) or human prompt |
| - | `action` | anything else | as action |

Rules live in `internal/messaging/triage` (`Classify`). A typed envelope stores
`attention` only when the sender set it; readers derive the default
(`EffectiveAttention`), so triage can tell an explicit `none` from no choice.

## Interrupt cap and batching

An action wake starts the seat's interval. A later action mail is deferred to
its end (the wake sweep parks it until then, without claiming the delivery):
the interval is a debounce, nothing is dropped. Human mail is never capped or
deferred.

Every wake of a triaged seat is **coalesced** (CW-20261117-0002): its text ends
with a digest of the seat's other pending, unread action and fyi mail, one line
each (id, sender, subject; replies to the seat are tagged `[reply to you]`, fyi
`[fyi]`), up to 15 lines and a count of the rest:

```
Also pending for you (3):
- `01a1…` @archie: Re: CW-20261115-0110 API: approved, build it [reply to you]
- `01a1…` @photon-review: #800 G6b verdict
- `01a1…` @nanite: Deploy done [fyi]
```

Once the wake is delivered, the action mail it named is marked **covered** (it
never wakes on its own; it was named in a delivered turn) and the fyi mail
**surfaced** (typed fyi also read). Covered mail stays unread in the inbox until
the agent reads it.

A triaged seat's old mail has no age bound in the wake sweep: coalescing makes
a backlog one turn, not a turn per message.

## Seat digest wakes

The wake sweep walks deliveries, so pending action mail whose delivery will not
be swept (a worker adapter already acknowledged it while triage held the wake,
or it sits behind a deep backlog) would never wake its seat. A seat pass after
each sweep (at most every 30 s) finds triaged seats with pending, unread action
mail older than 2 minutes and past any deferral, and gives each live, idle
session one **digest** turn listing all its pending mail (`Mailbox wake
(daemon-injected, digest)`); the listed mail is settled as above. It keeps the
seat's action interval. A busy or offline seat is retried on a later pass, so it
is woken once its session is idle again.

## Busy seats

A wake that finds its session mid-turn is released with a short backoff
(5 s doubling, capped at 60 s) and re-armed on the session's next turn end, so
it is delivered when the turn finishes however long the turn ran. Busy wakes no
longer count toward the 12-attempt give-up (which still applies to offline and
failed attempts).

## Backfill

On its first wake sweep after start the daemon re-classifies pending fyi mail
that is a reply to the seat's own mail as `action`/`reply_to_own` (idempotent;
logged and published once as `mail.triage_backfill {examined, moved, seat_count}`).
The sweep then wakes each affected seat once with a digest.

## FYI ride-along

On the seat's next human-originated prompt (Tachyon chat, or an operator turn;
never a mail wake, routed reply, another agent's turn, or a slash command such
as `/clear` or `/compact`), the hub appends one line:

```
FYI: 3 unread mail(s): @archie: Deploy done; @photon-review: Review posted; @x (tether_message_list to read them).
```

Once that turn was accepted, the listed mail is marked surfaced and typed fyi
is marked read; untyped fyi stays unread for the inbox.

## Events and records

- `mail.triaged {message_id, recipient, seat, handle, class, reason, typed, from}` once per message.
- Wake outcomes for mail that does not wake: `triaged-fyi`, `triaged-read`,
  `triaged-covered`, `deferred-cap` (in `wake.outcome`).
- Wakes made when mail is stored are now recorded as `mail_wake` turn inputs
  (they were `session_turn`), so Tachyon can fold them.
- Tables: `wake_triage` (one row per triaged message: class, reason, state,
  not_before) and `seat_wake_clock` (each seat's last action wake).

## Noise rules (CW-20261118-0050)

- **attention:none never wakes, triage on or off.** A typed envelope whose sender set
  `attention: none` is recorded and stays unread and visible, but no wake path (send,
  notify, sweep) wakes the seat for it, even a seat without `mail_triage`. The same action
  overrides apply (operator sender, `urgency: high`, a reply to mail the seat sent,
  `on_behalf_of`). With triage on it is also marked read and rides along in the next wake's
  digest; without triage it waits in the inbox and the next real wake's unread count names it.
  Wake outcome reason: `attention-none` (triage off) or `triaged-read` (triage on).
- **Acknowledgement replies are refused at send.** `POST /messages/post` (`tether_reply` /
  `tether_message_post`) rejects a reply of kind `update` or `message` whose body is only
  acknowledgement words (`Noted. No reply needed.`, `Thanks`, `ack`, `Got it, thank you`),
  with no answer, status, evidence, options or `?`, and whose typed parent was an `update` or
  `message` that did not await the replier. The sender gets `invalid_request` on field `body`
  saying to send nothing. Anything else passes (replies to a question or request, untyped
  parents, any body with a word outside the list, more than 10 words). Detector:
  `internal/messaging/ackfilter`. Off switch: `catalog.defaults.mail_triage.ack_filter: false`.
- **Wakes are coalesced.** One wake per seat per `action_interval` lists everything pending
  (see above); `TestBurstWithOneAskWakesOnce` pins a burst of 16 notices plus one ask to one wake.
- **Empty turns.** The wake text tells an agent with nothing to do to end the turn without
  producing any message (no acknowledgement), and never says to end silently when action is needed.

Measuring on a pilot: count `wake.outcome` events with `delivered: true` per seat, and
`mail.triaged` by `class`/`reason`, over equal windows before and after; the burst signature
is several delivered wakes within seconds to different seats from `attention: none` updates.
