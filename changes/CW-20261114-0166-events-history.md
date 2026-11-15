- Events history reads are indexed, bounded, and off the writer's connection (CW-20261114-0166).
  - **Index:** migration 0105 adds `idx_events_kind_id ON events(kind, id)`. A kind-filtered newest-first read on a copy
    of the hub DB went from 95.9 s (a full scan of 5.4M rows) to 5 ms. The index takes 10.5 s and 152 MB to build on
    the 12 GB hub DB, and runs once at the first start after the upgrade.
  - **Deadline:** `GET /events`, `GET /sessions/{id}/events` and `tether_events_history` read through
    `QueryEventHistory`, newest first in id windows, with a 5 s deadline. A read that runs out returns what it found,
    `"partial": true`, and a `next_cursor` to continue from; nothing is lost or repeated.
  - **Connection:** these reads use a separate read-only connection pool (2 connections, `query_only`), never the
    writer's single connection.
- `/health` gains `wal`:
  - `wal_bytes`: the size of the WAL file.
  - `checkpoint_seq`: the WAL header's checkpoint sequence number.
  - `last_checkpoint_age_sec`: how long that sequence has stood still. The daemon samples it every 15 s. It is a lower
    bound until a log restart has been seen (`last_checkpoint_observed`).
  A reader holding back checkpoints shows as a growing `wal_bytes` with a rising age.
