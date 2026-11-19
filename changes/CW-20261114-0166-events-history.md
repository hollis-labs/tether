- Events history reads are indexed, bounded, and off the writer's connection (CW-20261114-0166).
  - **Indexes, migration 0105** (measured on a copy of the 12 GB hub DB):
    - `idx_events_kind_id ON events(kind, id)`: a kind-filtered newest-first read went from 95.9 s (a full scan of 5.4M
      rows) to 5 ms. 10.5 s to build, 152 MB.
    - `idx_events_session_kind_id ON events(session_id, kind, id)`: without it the planner chose the kind index for
      `session_id=? AND kind=?` queries ordered or maxed by id, and walked every row of the kind (3.3 s for one old
      session's output); now 1 ms. 6.2 s to build, 346 MB.
    - Both build once, at the first start after the upgrade.
  - **Deadline:** `GET /events`, `GET /sessions/{id}/events`, `tether_events_history` and `tether_session_events` read
    through `QueryEventHistory` with a 5 s deadline. Session-filtered reads use the session index; other reads go newest
    first in id windows. A read that runs out returns what it found, `"partial": true`, and a `next_cursor` to continue
    from; nothing is lost or repeated, and a partial read is never reported as "no events".
  - **Connection:** these reads use a separate read-only connection pool (2 connections, `query_only`), never the
    writer's single connection.
- `/health` gains `wal` (CW-20261114-0166):
  - `wal_bytes`: the size of the WAL file.
  - `checkpoint_seq`: the WAL header's checkpoint sequence number.
  - `last_checkpoint_age_sec`: how long that sequence has stood still. The daemon samples it every 15 s. It is a lower
    bound until a log restart has been seen (`last_checkpoint_observed`).
  A reader holding back checkpoints shows as a growing `wal_bytes` with a rising age.
