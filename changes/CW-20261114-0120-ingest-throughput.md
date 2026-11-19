- Worker output ingest is faster under load (CW-20261114-0120).
  - **Group commit:** a session's `session.log` stays open and is fsynced once per 32 frames or 100 ms, not once per
    frame.
  - **Locking:** frames are serialized per session instead of under one global lock, so different sessions (several
    workers) ingest in parallel; each session's order is unchanged.
  - **Durability contract:** SQLite (the cursor and the `session.output` event) remains the record of what was
    accepted. On a crash, `session.log` may lose at most its un-synced tail, fewer than one batch; the cursor and
    events are unaffected. Codex permission-source sessions still sync every stdout frame before their byte spans
    commit.
  - **Load test:** `TestWorkerIngestLoad` runs small in CI and scales with `TETHER_INGEST_LOAD_FRAMES`,
    `TETHER_INGEST_LOAD_DIR` and `TETHER_INGEST_LOAD_DB`.
