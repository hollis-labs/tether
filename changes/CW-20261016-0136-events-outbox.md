- Events outbox, part 1 (CW-20261016-0136, X1a): every `events` row now carries a producer-minted
  `evt_` + 32 hex id (D6), persisted at insert (migration 0081, forward-only; existing rows keep a NULL
  id). `events.OutboxBus.PublishTx` writes a pending row on the CALLER's transaction (never a second
  connection; the database has one) and the bus publishes it after the commit, in `Seq` order and at least
  once, from a drainer that also recovers rows a crashed process committed but never published; a
  rollback publishes nothing; a repeat of an event id is the same row. Nothing uses it yet: the five
  in-transaction store writes move onto it in the next change.
