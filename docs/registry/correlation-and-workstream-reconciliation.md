# Registry correlation and workstream reconciliation (CW-20261120-0002)

## Authority and prerequisite audit

Registry must mint both workstream and correlation record IDs. Tether's existing
execution run ID, session ID and token jti are join keys; none may be reused as a
caller-selected Registry correlation record ID. This document is an integration
design and reconciliation plan only. It adds no client, launch hook,
configuration, ID allocation, registration request or migration.

Fresh source at deployed tools SHA
`7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523` supports `workstream/1` and
`correlation/1`. The actual contracts dependency is
`v0.0.0-20261120025733-4199eedcd2f4`; its schema registry includes both kinds.
Kind deployment alone does not establish the required minting operations.

The complete operation and HTTP route tables expose generic
`record_create` (`POST /v1/records`), but neither a server-minted workstream
ensure operation nor a server-minted correlation registration operation.
Generic create requires the caller's payload `id`; `prepare` constructs the
canonical record ID as `kind + ":" + payload.id`. The workstream schema requires
`schema`, `id`, `status`; the correlation schema requires `schema`, `id`, `name`,
`links`. Generic create does not allocate either identity.

Its idempotency receipt is scoped to caller, publication authority and request
key. It replays an identical request, rejects changed input under the same key,
and conflicts if the canonical record ID already exists. These semantics do not
substitute for Registry-owned minting or a cross-dispatch ensure contract.

Source evidence:

- [Routes](https://github.com/hollis-labs/tools/blob/7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523/registry/internal/catalog/transport.go#L22)
  and [operations](https://github.com/hollis-labs/tools/blob/7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523/registry/internal/catalog/service.go#L42).
- [Create receipts and existing-ID conflict](https://github.com/hollis-labs/tools/blob/7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523/registry/internal/catalog/service.go#L288)
  and [caller-supplied canonical identity](https://github.com/hollis-labs/tools/blob/7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523/registry/internal/catalog/service.go#L592).
- [Compiled contracts pin](https://github.com/hollis-labs/tools/blob/7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523/registry/go.mod#L12)
  and [pinned schema registration](https://github.com/hollis-labs/tools/blob/4199eedcd2f4/registry/contracts/schema.go#L24).
- [Workstream schema](https://github.com/hollis-labs/tools/blob/7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523/registry/contracts/schemas/workstream-1.json)
  and [correlation schema](https://github.com/hollis-labs/tools/blob/7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523/registry/contracts/schemas/correlation-1.json).

Both registration paths therefore remain gated on a Registry server contract.
No client-side UUID, execution-ID alias, create/read fallback or fabricated
ensure route should bypass that dependency.

## Correlation integration design after the mint contract exists

The agency Registry is separate from Tether's federation directory. This source
tree has no agency Registry HTTP client/configuration to reuse. Registry's CLI
uses an HTTP base URL and token-file convention; Tether already uses `credfile`
for private credential validation. Connection and authority configuration must
be agreed separately; this plan does not provision or enable credentials.

Registry must define and deploy a registration operation that accepts observed
join keys without a caller-selected record ID, allocates and returns its own
canonical correlation ID, and specifies idempotent replay/concurrency semantics.
It must also define authentication scopes, publication-authority binding,
reference admission and how late observations are conditionally attached. The
endpoint, request shape and update verb remain unspecified until that contract
is approved; generic `record_create` is not an implementation shortcut.

Tether can then schedule registration after successful local `Manager.Start` in
`internal/app/session_lifecycle.go`, or an exact authenticated worker launch
verdict in `internal/app/worker_sessions.go`. Network/credential failure must
never block or fail session start. Use bounded asynchronous work, request
cancellation/deadlines, redacted diagnostics and explicit retry/drop semantics.
Persist the Registry-returned correlation ID separately from all observed join
keys. Correlation association and compaction-lineage inheritance must continue
to reference the Registry-issued workstream ID unchanged.

An idempotency key may identify a launcher execution request; it must not become
the Registry record ID. Tests must prove the client omits caller-selected identity,
retains the server-returned identity, replays safely, bounds unavailable Registry
work, and never changes launch success into failure. Unknown worker outcomes are
not authenticated success verdicts.

The current sources available for a future allowlisted metadata representation
are:

| Observation | Actual source |
| --- | --- |
| `urn` | Launcher `runURN` in `internal/app/run_token.go` |
| `session` | Server-owned session row ID |
| Execution `run_id` | Launcher `TETHER_EXECUTION_RUN_ID`; not Registry correlation ID |
| Dispatch run ID | Session `RunID` is Torque runs.id; not a Tether team run |
| `task_id` | Server-owned session row, if present |
| `workstream_id` | Session row, including inherited value; must reference Registry authority after cutover |
| `jti` | Launcher-minted run-token claims, if handoff is supported |
| Provider session ID | Canonical `session_provider_mappings` observation, if available |

Run token bytes, bearer credentials and arbitrary environment/plan fields must
never enter Registry records or diagnostics. A legacy local workstream ID is an
observation, not evidence of Registry identity or existence; a prefix check alone
is not authority verification.

The current launch plan/session row does not carry manifest digest, export
receipt ID, ACTIVITY.md sha256 or traceparent. Their owners must expose explicit
observations before integration; do not infer them from a plan hash or inherited
environment. Provider IDs learned after start require the agreed conditional
update/reconciliation contract. Worker-native artifact metadata must be observed
on its owning worker, not guessed by the hub. Required follow-up tests cover
missing values, late provider observations and unsupported token handoff.

## Concrete store reconciliation plan

Current `internal/store/workstreams.go` still owns local creation in
`CreateWorkstream` (UUIDv7 if ID omitted) and `EnsureSessionWorkstream` (nearest
lineage match, otherwise create and stamp gaps). `AssignSessionWorkstream` checks
local existence. `inheritWorkstreamID` fills missing values at session creation;
explicit assignments win. This plan does not alter that behavior or rewrite
historical identities. It is the task's permitted reconciliation-plan deliverable,
not a completed authority cutover.

1. **Registry prerequisites, owned by Registry:** approve and deploy atomic,
   server-minted workstream ensure and correlation registration operations.
   Specify stable replay/concurrency semantics and return Registry-owned IDs.
   Neither generic create nor a Tether-generated ID substitutes for these verbs.
2. **Dispatch prerequisite, owned by Torque:** call the approved ensure operation
   and stamp its returned opaque Registry workstream ID on task, launch request
   and events. Preserve the full canonical ID. Establish the dispatch ensure key
   contract without moving ID authority into the caller.
3. **Tether mirror change:** add a local transactional import/upsert that accepts
   a verified Registry-issued ID, preserving local FK membership,
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

Implementation remains blocked on the two Registry mint contracts and Torque's
workstream stamping. This plan authorizes no runtime registration, migration,
Registry server change, shared configuration change or deployment.
