- Delivery and wake contract written down (CW-20261118-0024): `docs/messaging-wake-contract.md` separates stored, wake
  attempted, delivered, agent processing and read/consumed, and gives the supported watchdog invocation.
- A duplicate send (same idempotency key) or a repeated notify of a message whose wake was already admitted now reports
  the stored wake outcome. Before, a triaged seat answered `deferred-cap` (or `offline`) for a message that had been
  delivered, because triage and session resolution ran before the admission check. Still at most one turn.
- `tether_message_trace` adds `holder_kind` per attempt (`session` for a push wake, `recipient` for the receipts
  `mark_read`/`consume` record, whose `turn_submitted` is not a submitted turn), the recorded `wake_outcome`, and the
  recipient's `read_at`/`consumed_at`.
