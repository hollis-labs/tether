- Add server-side filters to the environment event stream (CW-20261020-0017):
  `GET /environment/events` and `GET /sessions/{id}/stream` accept `kind` (exact or
  `prefix*`), `category`, `subject` (prefix), `app`, `severity_min` and
  `scope.task_id|project_id|workstream_id|session_id|agent_urn|run_id|launch_id`.
  Filters never change the cursor, `synchronized` marker or gap behaviour, and
  `operator`-visibility events go only to the operator. `tether_events_wait` gains
  an environment-cursor mode (`after_seq` plus the same filters) that returns mesh
  events and a `next_cursor`; its existing arguments, `GET /events` and
  `GET /events/stream` are unchanged. See `docs/environment-streams.md`.
