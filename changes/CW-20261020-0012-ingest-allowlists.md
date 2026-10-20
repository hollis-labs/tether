- Event ingest sanitizes each kind with an allowlist derived from its catalog
  payload schema (CW-20261020-0012): only declared payload paths are stored, and
  kinds whose schema is free-form or composed fall back to the denylist scrub.
  Builds on `POST /events` (CW-20261020-0010).
