- Store a worker session's resume cursor and its `session.output` event in one transaction
  (CW-20261023-0018): `Store.RecordWorkerOutput` writes the `session_worker_seq` upsert and the event as a
  pending outbox row in one commit, and the bus publishes it after the commit. That is one producer
  transaction per output frame instead of two (the outbox drainer's mark-published update is a separate
  commit per drained batch, so total commits fall by less than half when frames arrive alone and more
  when they batch; to be measured), and the cursor can no longer record a frame whose event was
  lost. The cursor and event commit before the turn-output step runs, so a frame whose commit failed is
  replayed without having touched the in-memory result-line carry or published turn events. Subscribers
  now receive `session.output` from the outbox drainer, which the commit kicks, ahead of the turn events
  the same frame produces.
- Give `session.output` an explicit admission policy in the event pipeline (CW-20261023-0018), the same
  compatibility class as `session.turn_output`: the stored bytes are unchanged under `event_stage_mode:
  shadow` (the default, and what the hub runs). Under `enforce` the kind had no policy and its payload
  would have been replaced by `{"sanitize_error":"unknown_kind"}`; it is now kept.
