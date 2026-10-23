- Store a worker session's resume cursor and its `session.output` event in one transaction
  (CW-20261023-0018): `Store.RecordWorkerOutput` writes the `session_worker_seq` upsert and the event as a
  pending outbox row in one commit, and the bus publishes it after the commit. That is one database
  commit per output frame instead of two, and the cursor can no longer record a frame whose event was
  lost. The cursor still advances when the turn-output step fails, as before; the event is then not
  published. Subscribers now receive `session.output` from the outbox drainer, which the commit kicks.
