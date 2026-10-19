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
  unverified claim, no principal) has the field dropped: the launch still succeeds without a stamp and, for an identified
  caller, an `identity_audit` row with event `task_id_dropped` records it. A request with no principal is dropped silently
  on purpose: `identity/guard.go` forbids anonymous traffic from touching the database (and in enforce mode it never gets
  here). TODO: allow a Torque service principal here once the
  issuer provides one (CW-20261016-0017; decision `tether_task_id_stamp_and_usage_rows_2026_10_16`).
- **When.** At creation only. `POST /sessions` takes `task_id`; `POST /sessions/bootstrap` takes `task_id` and applies
  it only when the row is created. A later request, even from the launcher, never rebinds the session.
  Later claims about the task are Torque facts, not Tether state.
- **Where it goes.** The session row and the stored launch plan; `TETHER_TASK_ID` in the agent's environment, for
  hub-local and Outpost launches alike. At process launch, all three reserved variables (`TETHER_TASK_ID`,
  `TETHER_WORKSTREAM_ID`, `TETHER_PROJECT_ID`) come from the persisted session row. The hub seals the Outpost
  `LaunchSpec` environment; local and Outpost process launchers seal again after provider and ambient environment
  merging. Missing values remove inherited variables. Catalog, override, provider, and daemon-process values cannot
  supply a missing binding. These variables are context for the process, not proof for API attribution.
- **Resume and replacement.** A resume or team replacement continues the same work, so it inherits the stamp.
- **Attribution.** `callcontext.Snapshot.TaskID` is filled only when the caller is a verified session whose row was
  found (`SessionResolved`), so `tool_call_start`/`tool_call_end` attribution carries it for managed sessions and
  never for unmanaged ones. Missing is allowed; wrong is not.

## Who sends it today

- `tether launch --task <id>` stamps that task; without it, `--torque-task <id>` stamps its task when exactly one is given
  (several tasks: no single task, no stamp). An explicit `--task` wins.
  The CLI presents `operator.token`, so the daemon accepts it.
- The MCP `tether_session_create` tool deliberately has no `task_id`: agents are the usual MCP callers and would only be
  refused, so the stamp is set by the operator CLI (and later Torque's service principal), never through MCP.
- Torque's own launches wait for a Torque service principal (see the TODO in `internal/api/task_stamp.go`).

## Event propagation and source acceptance

`session.turn_output` carries optional `task_id` alongside project/workstream in its payload, envelope scope, and
routed-message metadata. The durable output retry format remains unchanged: publication rereads the session row
for the task binding and current workstream, including after restart. Native event scope cannot replace these
session facts. A metadata read timeout keeps the output queued; an unavailable row does not invent a task.

Canonical `tool.call.started`/`tool.call.finished` retain their published metadata-only payload schemas. Their
correlation belongs in envelope scope, supplied by verified session attribution and the existing session-row stage;
unresolved claims never become verified attribution. Legacy tool-call attribution snapshots retain the same fields.

Resume/replacement reuse the stored task, project resolution, and existing workstream lineage inheritance.
Bootstrap creates an external identity row and accepts a task only through the existing operator policy; repeated
bootstrap cannot rebind it. It does not launch a process or establish verified MCP attribution. Missing project or
workstream on an external row stays missing rather than being inferred from a task, directory, or transcript.

Source acceptance covers these propagation paths and focused synthetic checks. Owner live activation and rollback
verification are separate deployment evidence. No historical task inference/backfill or new service-principal
credential/issuer authority is part of this change; Torque service launch authorization remains held as above.
