- Add `POST /events`, the generic publish route for apps (CW-20261020-0010): one
  runtimeevents v1.1 envelope or a batch of up to 100, answered per event
  (`accepted`, `duplicate` with the original cursor, or `rejected` with a code), idempotent on the
  event id with `id_conflict` for a different event reusing it. Off until
  `daemon.event_ingest.enabled`. Publishing needs `tether.events` under identity
  enforce, service credentials are bound to their app, kinds follow the libs
  `producers.json`, payloads are scrubbed with the shared redaction engine, and a
  new `event_ingest_digests` table (migration 0095) holds the dedupe evidence.
  See `docs/event-ingest.md`.
