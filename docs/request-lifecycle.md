# Request tasks (CW-20261120-0177)

A typed `request` on a Torque-connected Tether hub creates a manual Torque task
before sending mail. The response and stored envelope carry `task_id`. Basalisk
uses the same `POST /messages/post` endpoint as `tether_ask`; this is not an MCP-only
feature. Follow that task to see `metadata.request`, acknowledgment time, status
comments, and the final summary. Mail delivery receipts remain separate from a
recipient claiming the work.

## Sending and claiming

Use `tether_ask` with `kind: request`, one recipient, `subject`, `body`,
`deliverable`, and a stable `idempotency_key` (required for tracked submissions and final replies). Omit `task_id` to create a task, or
supply an existing open manual task explicitly to link it. `re: task:...` remains
conversation context; it does not implicitly turn that task into the request.
Multi-recipient tracked requests are refused: submit one per recipient.

The recipient posts `STATUS started` on the returned Torque task, or uses Torque's
explicit request transition to `acknowledged`. Torque records `ack_at`. Transition
to `in_progress` when work begins. Post progress only as task comments when the
state changes. Reading or delivering mail does not acknowledge the work.

Finish with `tether_reply`, the original message's `in_reply_to`, a summary body,
`status: done|declined|blocked`, and evidence pointers. The daemon verifies that the
sender is the original request recipient. Torque stores the final summary and
request state. `blocked` closes this request obligation; follow-up work is a new
request. The lifecycle is independent of Torque's ordinary task status vocabulary.

## Alerts

Torque owns the persisted deadline: fifteen minutes after submission when the
recipient is running, immediately when no running session resolves. The hub checks
live session/outpost routing, not a seat's declared status. A human recipient is
not considered an offline agent.

The offline path attempts an alert immediately after sending the request. A daemon
sweep checks durable overdue rows every five seconds, including after restarts.
Alerts go to the requester's canonical mailbox; an owner request therefore alerts
`msg://user/local/operator`, the owner mailbox consumed by Basalisk. The watcher
uses a notice, not another request requiring acknowledgment.

Mail uses the stable key `request-unacknowledged:<task_id>` and an immutable
payload. The watcher marks the alert in Torque only after mail is durably queued.
A failed send stays eligible; a failed mark retries the same mail without duplicates.
Torque publishes `request.submitted`, `request.acknowledged`,
`request.state_changed`, `request.unacknowledged_alert`, and `request.closed`.
The alert event carries `reason: recipient_offline|ack_timeout`, based on the
persisted submission-time liveness snapshot. Event payloads use `requester_urn`,
`recipient_urn`, and `mail_message_id`; the final summary is read from task comments.

## Configuration and rollout

Configure **one hub** with `TORQUE_BASE_URL` (for example
`http://127.0.0.1:8990`) and a daemon Torque service credential in
`HOLLIS_AUTH_TOKEN_FILE_TORQUE`. The private credential file is read on every call;
`TORQUE_API_TOKEN` is a compatibility fallback when no file is set. Do not put
credentials in URLs. The service credential needs the Torque request read/write
routes. Without `TORQUE_BASE_URL`, standalone Tether retains its existing mail-only
behavior and runs no request sweep. Do not enable competing independent hubs for
the same Torque request queue.

Land the Torque request API before enabling the Tether hub configuration. Shared
service restarts/cutovers belong to the Lead Orchestrator. This change does not
restart or deploy services.

A configured but unavailable Torque fails request submission before sending mail.
Task creation and request registration are one idempotent Torque operation. If
mail was sent but attaching its actual ID failed, or a reply was mailed but its
final Torque transition failed, the API returns an error naming the completed
mail step. Retry the identical operation with the same idempotency key. Final replies have a transport identity derived from the task, so changing the
caller key cannot publish a different final outcome. A retry
repairs the remaining step without duplicating the task, mail, summary, or events.

## Skills and follow-up

The example `ask` and `mailbox` skills in `examples/catalog/skills` describe this
flow, including the recipient rules already in `work-reporting` §6. They can be
used when updating the live catalog. The full 90-skill/boot-charter audit and live
catalog render verification in CW-20261117-0005 remain separate: that repository
and deployment are outside this unit's two-repository scope.

Future activity snapshots can join `task_id`, `recipient_urn`, and
`mail_message_id` to session activity. Long-running acknowledged work does not
trigger this unacknowledged-request watcher.
