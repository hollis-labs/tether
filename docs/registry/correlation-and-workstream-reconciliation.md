# Registry correlation and workstream reconciliation (CW-20261120-0002)

## Correlation registration

The agency Registry is separate from Tether's federation directory. This checkout
had no agency Registry HTTP client or configuration to reuse. The optional
`daemon.correlation_registry` configuration adds its base `url` and private
`token_file`. It follows the existing `credfile` credential checks used for MCP
upstreams; it does not reuse a launched session's bearer token or run token.
The operator must supply an existing Registry credential with record-write scope
and an owning publication-authority binding. No credential is provisioned here.
An empty URL disables the integration. Catalog daemon configuration is retained
across reloads, so changing it requires a daemon restart.

After a successful local Manager start or authenticated worker launch verdict,
Tether schedules one best-effort `POST /v1/records` (`record_create`). There are at
most eight in-flight jobs, a five-second request deadline, no retries or durable
outbox, and no wait in the launch path. Saturation, credential errors, timeouts
and non-success responses produce secret-free diagnostics and cannot fail start.
Redirects are refused. Crash-before-send or unavailable Registry can lose the
observation; a later reconciliation/outbox task is needed for guaranteed delivery.
Failed starts do not register. Unknown worker outcomes are not success verdicts.

The `correlation/1` payload has execution run ID as `id`, a generated display
`name`, a `session` reference and available agent/task/Registry-workstream links.
The record human key and idempotency key are `tether-execution-<execution run ID>`.
These replay exactly the same observation; changed observations are not updates.
`metadata` carries the allowlisted observation fields:

| Field | Actual source |
| --- | --- |
| `urn` | Launcher `runURN` (logical agent, otherwise session principal) |
| `session` | Server-owned session row ID |
| `run_id` | Fresh launcher `TETHER_EXECUTION_RUN_ID`, not the Torque run ID |
| `dispatch_run_id` | Launcher-asserted Torque runs.id from session row; not a team run |
| `task_id` | Server-owned session row, omitted if unavailable |
| `workstream_id` | Session row, including inherited value; never minted here |
| `jti` | Verified claims of the launcher-minted run token, if handoff supported |
| `provider_session_id` | Canonical provider mapping available when job runs |

A legacy local workstream ID stays metadata; only an already prefixed
`workstream:` ID becomes an authority=`registry` link. This is a format guard,
not a remote existence verification. The Registry create contract accepts opaque
references; correlation registration does not fetch or ensure workstreams.
Run token bytes, credentials and arbitrary environment/plan fields are excluded.

The current launch plan/session row does not carry manifest digest, export
receipt ID, ACTIVITY.md sha256 or traceparent. No such values are inferred from
similarly named plan hashes or inherited environment. Those require the owning
manifest/export/launcher contract to hand Tether explicit observed values before
this allowlist is extended. Provider IDs discovered after registration need a
separate conditional update/reconciliation operation; this snapshot does not
wait for a provider callback. Worker-native artifact metadata lives on the worker
and is not guessed by the hub. Torque run ID remains available in the session row
and should not be confused with execution run ID.

## Concrete store reconciliation plan

Current `internal/store/workstreams.go` still owns local creation in
`CreateWorkstream` (UUIDv7 if ID omitted) and `EnsureSessionWorkstream` (nearest
lineage match, otherwise create and stamp gaps). `AssignSessionWorkstream` checks
local existence. `inheritWorkstreamID` fills missing values at session creation;
explicit assignments win. This change does not alter that behavior or rewrite
historical identities. It implements the task's permitted reconciliation-plan
option, not a completed authority cutover.

1. **Registry prerequisite, owned by Registry:** expose the approved atomic,
   server-minted workstream ensure operation. Deployed tools SHA
   `7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523` has the schemas and generic
   create, but no such verb. Generic create's exact-request receipt replay and
   caller-supplied payload ID do not substitute for ensure.
2. **Dispatch prerequisite, owned by Torque:** call that operation and stamp the
   returned opaque Registry ID on task, launch request and events. Do not strip
   its `workstream:` prefix. Establish the stable dispatch ensure key contract.
3. **Tether mirror change:** add a local transactional import/upsert that accepts
   an externally supplied verified Registry ID, preserving local FK membership,
   name/status/project associations and compaction lineage. New dispatched
   launches must import/reference that ID before creating the session; they must
   never call the UUID-allocating path. Keep standalone legacy creation explicitly
   separate until its consumers migrate.
4. **Legacy reconciliation:** inventory local workstreams and all referencing
   sessions/events, then build a reviewed old-to-Registry-ID mapping. Treat
   workflow/name similarities as candidates only. Keep an immutable alias mapping
   for historical joins; do not rewrite existing event history. Import referenced
   Registry rows and rewrite session/local membership atomically with uniqueness
   and FK checks after backup and a reviewed dry run. Conflicting lineage
   assignments require explicit mapping, never silent parent overwrite.
5. **Retire authority:** switch API/CLI/MCP create/ensure consumers to the Registry
   operation, make local creation reject missing Registry ID in managed mode,
   and retain mirror reads plus historical aliases. Add tests proving replay,
   concurrent dispatch convergence, explicit-ID precedence, nearest-ancestor
   inheritance, gap filling without overwriting, and compact/resume/fork chains.
   Existing `internal/store/workstreams_test.go` and
   `workstream_namespace_test.go` are the regression suite for these invariants.

No migration, new workstream authority, Registry server change or deployment is
part of this PR. Authority cutover is blocked on steps 1 and 2; registration can
operate independently using whatever IDs the session already has.
