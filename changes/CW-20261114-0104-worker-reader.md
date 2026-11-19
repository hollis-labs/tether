- A slow hub ingest no longer wedges every command to a worker (CW-20261114-0104). The hub routed a worker's command
  responses (turn acks, mail-wake deliveries, stops) on the same goroutine that ingested its session output (log fsync,
  SQLite, turn-output persistence, a `last_seen` write per frame). Under load that ingest fell behind, every command
  timed out at 30s while the worker stayed connected, and only a hub restart cleared it. The reader now only routes
  responses: session frames go to a per-connection ingest goroutine through a bounded queue (overflow is dropped and
  replayed from the stored cursor once it drains), worker bind/unbind/activity to a control goroutine, and `last_seen`
  is written from the ping ticker. A worker that leaves 3 commands in a row unanswered publishes
  `worker.delivery_degraded` (`action: reported`); at 6 the hub closes its connection so it reconnects and resumes
  (`action: reconnect`). `tether workers` shows `delivery_timeouts`, `ingest_backlog` and `dropped_frames`.
