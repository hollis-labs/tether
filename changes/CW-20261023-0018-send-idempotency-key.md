- Add an idempotency key to message send (CW-20261023-0018): `--idempotency-key` on `tether messages
  send` and `notify`, an `idempotency_key` property on `tether_message_send` and `tether_message_notify`,
  and an `Idempotency-Key` header on `POST /messages` and `/messages/notify`, all mapped onto the
  `tether.message_id` metadata the delivery store already deduplicates on (scoped by sender, a changed
  body is `409`). The Go client now sends a fresh key per call and repeats a send that timed out or
  lost its connection with the same key, so a retry after a timeout cannot store the message twice.
  Found when a slow hub made clients time out on sends the server had already committed.
