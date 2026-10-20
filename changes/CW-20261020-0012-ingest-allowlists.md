- Event ingest sanitizes each kind with an allowlist derived from its catalog
  payload schema (CW-20261020-0012): only declared payload paths are stored, and
  kinds whose schema is free-form or composed fall back to the denylist scrub.
  Builds on `POST /events` (CW-20261020-0010).
  A schema that is declared but broken (unreadable, not JSON, or a policy that
  does not compile) is refused at daemon start, naming the kind and schema; it
  never degrades to the denylist.
