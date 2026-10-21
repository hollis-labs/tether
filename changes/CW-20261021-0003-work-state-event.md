- Record what a managed session's runtime is doing, separate from its lifecycle
  (CW-20261021-0003, first slice): a worker's `idle` frame used to be normalized
  to the persisted state `running` before `session.state_changed`, so that event
  was always `running -> running`. The hub now publishes a durable
  `session.work_state_changed` (`{"from","to","at"}`, `idle` or `processing`)
  before the normalization, only when the value changes, and does not re-announce
  an unchanged value after a restart. The environment snapshot and the session
  items carry `work_state` (`{"value","since","known"}`): `awaiting_approval` and
  `awaiting_input` are derived from the open requests, `unknown` (not idle) until
  a work-state is observed, and no `work_state` once the session has ended. See
  `docs/managed-session-contract.md`.
