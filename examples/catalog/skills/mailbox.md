---
id: mailbox
name: mailbox
description: Read durable Tether requests and reply with results.
---

Read your inbox with the current Tether mailbox tools. A delivered message is not
yet a claimed request. For a request with `task_id`, post `STATUS started` on that
Torque task promptly or explicitly claim it through Torque. This records the
acknowledgment time and stops the 15-minute unclaimed alert. Transition the request
to `in_progress` as work begins. Progress belongs in task comments on state change.

Finish with exactly one `tether_reply` using the original request's `in_reply_to`,
a concise final summary, `status: done|declined|blocked`, evidence pointers, and a
stable `idempotency_key`. The reply updates the linked Torque task and records its
final summary. Retry the identical reply with the same key if the response reports
that mail succeeded but the lifecycle update failed. Do not answer FYIs or send
acknowledgment/status chatter by mail.
