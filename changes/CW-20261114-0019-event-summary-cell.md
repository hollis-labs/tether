- Events carry a one-line `summary` and the seat's `scope.cell` (CW-20261114-0019). Migration 0099 adds the `summary` and
  `scope_cell` columns (nullable, no backfill). A new stamp stage fills them on every event Tether writes: the cell from the
  catalog agent's new `cell:` field (inherited through `extends`), the summary from the kind and low-risk payload fields,
  redacted, one line, at most 200 characters. `/environment/events` and `tether_events_wait` return `summary` and
  `scope.cell` and filter on `scope.cell` / `cell`; `GET /events` and `tether_events_history` return `summary` and `cell`.
  See `docs/event-vocabulary.md`.
- New kinds (CW-20261114-0019): `session.cleared` (the hub submitted `/clear`), `session.compacted` (a worker-hosted Claude
  session's `compact_boundary`), `seat.waiting` / `seat.unblocked` (derived from `session.request_status`). `message.read`
  gets a sanitize policy and a summary.
- Raw `session.output` goes to the `R-debug` retention class (CW-20261114-0019): the daemon admits it at start, and the
  hourly sweep deletes R-debug rows after `daemon.events_retention.debug_days` (default 7). Other kinds and rows written
  before the change stay on the 90-day window; nothing is backfilled.
