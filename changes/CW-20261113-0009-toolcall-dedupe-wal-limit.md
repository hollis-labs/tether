- Log each tool call once, as the canonical pair, and keep the WAL file bounded (CW-20261113-0009).
  - **Tool calls:** the daemon no longer writes the legacy `tool_call_start`/`tool_call_end` pair to the `events`
    log; `tool.call.started`/`tool.call.finished` (the catalog kinds Tachyon and the Torque console read) are the
    only rows, two per call instead of four. The completed call's full legacy payload stays in `proxy_events`
    (`tether_events_tool_calls`). `POST /proxy/events` with `publish` publishes the canonical kind.
  - **Readers:** `tether_tool_metrics` reads `tool.call.finished`, plus legacy `tool_call_end` rows written before
    this change, counting each call once. `tool.call.finished` now carries `discovery_mode`. The session reaper
    counts the canonical kinds as activity.
  - **Retention:** both canonical kinds are admitted as `R-activity` (still on the main events window).
  - **WAL:** the writer connection sets `journal_size_limit` to 64 MiB, so the `-wal` file shrinks back whenever the
    log restarts instead of staying at its largest size. The daemon's WAL sampler also runs a `TRUNCATE` checkpoint
    (250 ms busy wait, at most once a minute) while the file is over 256 MiB.
