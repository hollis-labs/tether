- Worker output frames already queued for one worker are committed together (CW-20261115-0027).
  - **Draining:** the hub's ingest loop takes the output frame in hand and every output frame already waiting behind it
    (up to 64), with no waiting for more. The app commits every accepted frame's cursor and `session.output` event in
    one SQLite transaction, still under `synchronous=FULL`, and runs each frame's turn-output step only after that
    commit.
  - **Order:** a state frame (or any non-output frame) ends the run and is ingested after it.
  - **Failures:** a frame that is not accepted is replayed from its session's stored cursor, exactly as before. The
    hub's queueing, overflow and replay behaviour is unchanged.
  - **Durability:** a frame counts as accepted only once its batch commits. A crash before the commit loses nothing
    accepted, because the worker replays the batch from the stored cursors.
