# Registry workstream reconciliation plan

Status: reconciliation design, with the launch-registration slice implemented
by CW-20261121-0030. See [launcher correlation registration](launch-registry-correlation.md)
for the current runtime contract, configuration and best-effort limitations.
The broader historical migration and retirement plan below remains future work;
its original prerequisite notes describe the state when the plan was authored.

The approved design (Tesseract item `01M7TVAPAJ9MX44KPQTQK10NM5`, revision 4,
section 5 and decision D4) assigns workstream and correlation identity to the
agency Registry. Torque dispatch ensures a workstream through one idempotent
Registry operation. Tether retains session lineage and digest behavior, with
rows referencing the Registry identity. Agents do not create workstreams.

## Current implementation

- [The workstream store](../internal/store/workstreams.go) generates UUIDv7
  IDs in `CreateWorkstream` when no ID is supplied. `EnsureSessionWorkstream`
  resolves self, then the nearest ancestor, then creates a local workstream.
  `stampLineage` fills gaps without overwriting a different association.
- [Migration 0023](../internal/store/migrations/0023_workstreams.sql) stores
  the local ID in `workstreams.id` and `sessions.workstream_id`.
  [Project membership](../internal/store/migrations/0090_workstream_projects.sql)
  also refers to it. Replacing primary keys would change historical joins.
- `inheritWorkstreamID` inherits on session creation from `parent_session_id`,
  independently of intent; an explicit association wins. Missing parents,
  cycles, dangling references and divergent lineage have established handling.
- [HTTP](../internal/api/workstreams.go), CLI and MCP expose local create,
  ensure, assignment and updates. Removing only UUID generation is insufficient.
- [Session correlation](../internal/app/session_correlation.go) and
  [environment sealing](../internal/launch/correlation.go) use session facts.
  [Run-token creation](../internal/app/run_token.go) mints an execution-run ID
  and JTI. That execution-run ID is distinct from the Torque run on the session;
  [existing tests](../internal/app/run_token_test.go) preserve this distinction.
- [Digests and namespaces](workstreams.md) depend on session associations.
  Workstream identity is an attribute, not a Tesseract namespace partition.
  The federation directory in `internal/registry` is not the agency Registry.

## Prerequisites

Audited Registry build `7dd933d7c48edfd8b85d6d3ca34e1b3dcf691523` contains
`workstream/1` and `correlation/1`. Its generic `record_create` requires a
caller-supplied payload ID, replays identical keyed requests, and conflicts on
an already claimed ID. This is not server-owned mint/ensure. Do not substitute
client-generated IDs or a get/create/conflict sequence.

CW-20261120-0090 must settle the workstream key domain, authority/ownership,
server-minted identity, concurrent ensure, retry-key retention, closed/deleted
handling, and correlation identity/lifecycle. CW-20261120-0002 comment 27820
contains proposed wire shapes and explicitly unresolved choices; it is not an
approved API contract.

Separately authorized deployment and runtime verification must prove the
approved operations and authorized catalog/schema reads for both kinds.
Source presence, a healthy process or a merged server PR is not live acceptance.
Deployment and credential/grant changes are outside this task.

## Reconcile without re-keying history

1. Add a forward migration for an explicit nullable Registry reference on a
   local workstream row; never edit landed migrations. Keep existing primary
   keys and session/project/ref joins unchanged. Unmapped rows remain explicitly
   legacy. Exact columns, constraints and any mapping relation belong in the
   reviewed implementation after the Registry key-domain decision.
2. For new dispatches, Torque obtains the canonical ID from Registry before
   launch. Tether admits it through a trusted dispatcher contract, then creates
   or reuses a local reference row transactionally. New rows should use the
   canonical ID directly where the agreed contract permits. An internal storage
   key must never become a second public ID authority. Validate admission, not
   merely the string prefix.
3. Expose a canonical Registry reference separately wherever a legacy local ID
   can also occur. Update launch requests, session correlation, event stamping,
   digests, reverse lookup, project membership and namespace responses together
   before activating new dispatches. Never stamp an unmapped legacy value as a
   verified Registry identity.
4. Preserve nearest-ancestor resolution and never-overwrite behavior. Carry an
   admitted association through compaction/resume/fork using local durable data,
   without a new Registry dependency for continuation. An unmapped legacy
   lineage stays legacy until an explicitly reviewed mapping is available;
   continuation must not silently adopt a different canonical workstream.
5. Retire local minting on every write surface at activation. Unbound ensure
   requests report that dispatcher-owned assignment is required instead of
   creating a UUID fallback. Define the API/CLI/MCP compatibility errors
   together. Local mutation of Registry-owned metadata must also stop or use
   an explicitly authorized Registry contract.

Do not map by display name, workflow label or coincident string. Existing unit
plans use free-text labels; equal labels do not prove shared identity across
projects or authorities. Mapping old rows, merging containers, re-keying
sessions and rewriting historical events require a separately approved
migration. This plan performs none. The task's limited historical traceparent,
provider-session and manifest backfill does not authorize workstream-ID backfill.

## Dispatch and asynchronous correlation

Torque persists the Registry reference before team formation or individual
session launch. The formation snapshot retains it across ambiguous responses
and crash retries. Tether receives dispatcher-authorized task/run/workstream
facts before emitting launch events; binding only after creation is insufficient.

After local launch facts commit, queue bounded best-effort registration through
the approved Registry minting operation. Slow/unavailable Registry, exhausted
retries and authorization errors must not delay or fail session start. Persist
enough retry state to reuse registration identity after restart, without storing
the run token; record only JTI. Bound queue capacity, request duration and
shutdown work. Make failures observable without exposing secrets.

Distinguish agent URN, session, signed execution run, Torque run/team run, task,
canonical workstream and JTI. The server contract must settle field names and
correlation lifecycle first. Preserve available traceparent, provider-session
ID, manifest digest, export receipt and ACTIVITY digest with documented
provenance. Missing facts stay absent; do not fabricate them or import unrelated
ambient claims. Late enrichment updates the same correlation identity under an
agreed conditional update contract.

## Acceptance and recovery

Implementation PRs must demonstrate:

- Concurrent/replayed dispatch, lost response and restart converge on one
  Registry ID; conflicting keys and authorities never merge identities.
- Tasks, team members, individual sessions and their first launch events carry
  the admitted reference. Caller/env spoofing cannot replace it. Execution-run
  and Torque-run IDs remain distinct.
- Fresh, compacted, resumed, forked and sibling sessions retain association;
  divergent ancestors are never overwritten. Missing-parent, cycle and dangling
  reference cases retain coverage.
- A seeded legacy database migrates without changing primary keys, historical
  associations, project joins, refs or digest output. Unmapped state is explicit.
- All local create/ensure surfaces stop minting after activation; fake or
  unadmitted Registry references are rejected.
- Hung/failing Registry and a full retry queue do not delay startup; retries,
  restarts and late enrichment never create duplicate correlations.

Activation requires reviewed source, green CI and separate live API/catalog
receipts. Keep unmapped lineage readable. Before future data changes, verify a
backup and rehearse on a scratch copy. Rollback must preserve admitted Registry
references and refuse a return to local minting; do not blindly downgrade the
database or re-key sessions. Document a compatible binary/migration rollback
boundary in the implementation before deployment.
