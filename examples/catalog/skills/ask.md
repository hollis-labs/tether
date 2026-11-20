---
id: ask
name: ask
description: Ask someone for an answer or action, using a typed Tether message.
---

Use `tether_ask` for an answer or action that another person or seat must provide.
For assigned work use `kind: request`, exactly one recipient, a clear subject,
brief body, a deliverable, and a stable `idempotency_key`.

On a Torque-connected hub the request becomes a manual Torque task. The returned
`envelope.task_id` is its durable reference. Supply `task_id` only to link an
existing open manual task; `re` provides conversation context. Keep the returned
mail ID for the reply. Retry unchanged input with the same key after a partial
failure. Follow the task for acknowledgment and progress, not status mail.

For questions or decisions, use the corresponding kind with concise options.
An unanswered request or timeout never grants approval. Use sensible reversible
defaults within your authority rather than asking unnecessary questions.
