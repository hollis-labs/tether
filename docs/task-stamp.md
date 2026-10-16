# Launch-time task stamp

A session can carry the Torque task it was launched for. The stamp is one opaque string on the session row,
`sessions.task_id` (migration 0079), so cost and activity can be tied to a task without regexes over transcripts.

## Contract

- **Value.** The bare Torque id, for example `CW-20261016-0155`, never transformed. The URN form
  `msg://task/torque/<id>` is used only where a URN is needed. Accepted shape: letters, digits, `.`, `_`, `-`,
  starting with a letter or digit, at most 64 characters (400 otherwise). There is no existence check against Torque.
- **Who may assert it.** Only the verified local operator (`identity.IsOperatorCredential`), as an explicit allow:
  a new principal kind starts denied. An agent or a session credential that sends `task_id` gets
  `403 task_id_not_permitted`. Any other caller (a worker device, another service, an operator-minted narrow token, an
  unverified claim, no principal) has the field dropped: the launch still succeeds without a stamp and an
  `identity_audit` row with event `task_id_dropped` records it. TODO: allow a Torque service principal here once the
  issuer provides one (CW-20261016-0017; decision `tether_task_id_stamp_and_usage_rows_2026_10_16`).
- **When.** At creation only. `POST /sessions` takes `task_id`; `POST /sessions/bootstrap` takes `task_id` and applies
  it only when the row is created. A later request, even from the launcher, never rebinds the session.
  Later claims about the task are Torque facts, not Tether state.
- **Where it goes.** The session row and the stored launch plan; `TETHER_TASK_ID` in the agent's environment, for
  hub-local and Outpost launches alike (the Outpost gets it through the `LaunchSpec` env, so no worker change or feature
  flag is needed). The variable is set last: a catalog, agent or `override.env` value for `TETHER_TASK_ID` is replaced
  by the stamp, or removed when the launcher named no task, so the agent cannot spoof it through its own environment.
- **Resume and replacement.** A resume or team replacement continues the same work, so it inherits the stamp.
- **Attribution.** `callcontext.Snapshot.TaskID` is filled only when the caller is a verified session whose row was
  found (`SessionResolved`), so `tool_call_start`/`tool_call_end` attribution carries it for managed sessions and
  never for unmanaged ones. Missing is allowed; wrong is not.

## Who sends it today

- `tether launch --torque-task <id>` stamps that task when exactly one is given (several tasks: no single task, no stamp).
  The CLI presents `operator.token`, so the daemon accepts it.
- The MCP `tether_session_create` tool takes an optional `task_id`. It is honored only when the MCP server presents the
  operator credential; an agent session gets `task_id_not_permitted`.
- Torque's own launches wait for a Torque service principal (see the TODO in `internal/api/task_stamp.go`).

## Not yet

`task_id` on `session.turn_output` (CW-20261012-0062b) and any backfill of older sessions (0062c) are separate.
