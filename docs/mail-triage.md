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

| Class | When (first match) | What happens |
| --- | --- | --- |
| `human` | from the operator; envelope `attention: human` | wakes now, never capped |
| `read` | envelope `attention: none` (marked read by the hub, `message.read`); untyped `urgency: very-low` (left unread) | no wake |
| `action` | `urgency: high`; envelope `awaiting` this seat; envelope `question`, `decision`, `approval_request`, `escalation`, `handoff`; untyped `request`, `escalation`, `handoff` | wakes, at most once per `action_interval` per seat |
| `fyi` | envelope `update`, `message`; untyped `notice`, `status_update`, `response` | no wake; rides along on the next human turn |

Rules live in `internal/messaging/triage` (`Classify`).

## Interrupt cap and batching

An action wake starts the seat's interval. A later action mail is deferred to
its end (the wake sweep parks it until then, without claiming the delivery). A
delivered wake of the seat, action or human, marks its other pending action
mail **covered**: the wake text already counts every unread message, so they do
not wake again. Human mail is never capped or deferred.

## FYI ride-along

On the seat's next human-originated turn (Tachyon chat, or an operator turn;
never a mail wake, routed reply or another agent's turn), the hub appends one
line:

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
