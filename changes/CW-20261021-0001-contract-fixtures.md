- Publish JSON Schemas and fixtures for the managed-session contract
  (CW-20261021-0001): `docs/fixtures/managed-session` holds the snapshot, a
  single session, one event per kind and the SSE frames (`synchronized`, `gap`)
  generated from the real types and handler by golden tests, JSON Schemas for the
  session, the snapshot and the event envelope validated against every fixture,
  and schema-valid examples of the PROPOSED additions. Test and documentation
  files only; no behavior change.
