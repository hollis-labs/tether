- Make `GET /environment/snapshot` cheap and its failures diagnosable
  (CW-20261021-0002): the per-session "last event" time is now a `MAX(at)` index
  lookup instead of `ORDER BY id DESC LIMIT 1`, which read and sorted every event
  of every session on each call (10 s on the first call against the production
  database, 1.9 s warm, about 0.6 s on an idle copy; about 0.16 s after). A failed
  durable read is now classified: a busy database, a deadline or a gone client is
  `503` with `Retry-After: 2` and "unavailable: temporarily busy, retry"; any
  other failure stays `500` with the generic body. The cause is logged
  (`environment <snapshot|event tail> read failed: class=...`), never sent to the
  caller. The same applies to the per-session snapshot and the stream's tail read.
