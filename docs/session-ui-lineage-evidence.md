# UI lineage evidence (CW-20261115-0084)

Authenticated item `GET /sessions/{replacement}` adds three nullable details.
Collection reads do not populate them. These fields describe verified
execution lineage for UI plan/history reconciliation. They confer no launch,
read, native-context, credential or runtime-custody authority. In particular,
none can satisfy the native continuation API's `native_admission` requirement.

## Retained local-team replacement

`replacement_lineage` is null without a committed, eligible replacement; otherwise:

```json
{
  "kind": "retained_team_v1",
  "source_session_id": "source",
  "replacement_session_id": "replacement",
  "logical_agent_id": "same-seat",
  "intent_key": "retained-session-intent",
  "actor_uri": "msg://agent/authority/member",
  "source_actor_uri": "msg://agent/authority/member",
  "replacement_actor_uri": "msg://agent/authority/member",
  "binding_id": "retained-binding",
  "binding_generation": 1,
  "committed_at": "2026-11-19T18:00:00.000000001Z"
}
```

The projection reads existing immutable `session_replacements`, not user input.
It verifies source/replacement IDs, same nonempty logical agent, both local
placements, explicit parent/resume, the source plan's committed digest, both
plans' team marker, and the destination's saved recovery actor/source. The
source actor comes from the original session-port request, independently checked
against the enrollment's acquired/payload actor and retained binding actor and
generation. A nonempty source recovery actor must agree too. An initial source
need not have a recovery actor: its enrolled identity lives in the original
session-port request. The source and replacement actor fields therefore expose
independently checked stored identities, not copies of a client assertion.

The source must be settled; orphaned is eligible only for this committed retained
replacement proof. The destination may be running or a settled historical
intermediate. Unstarted/detached/unknown attempts yield null. This supports
A→B→C after B has ended and the retained binding moved again; the consumer must
still validate every edge, require a currently running tip, enforce same-agent
and Continue/clear boundaries, and commit local lineage/plan changes atomically.
A malformed or conflicting committed record fails the read explicitly.

## Legacy worker relaunch

`worker_lineage` is null without both explicit creation provenance and a positive
authenticated launch-command acknowledgement; otherwise:

```json
{
  "kind": "worker_lineage_v1",
  "source_session_id": "source",
  "replacement_session_id": "replacement",
  "logical_agent_id": "same-seat",
  "source_worker_id": "source-placement",
  "worker_id": "replacement-placement",
  "command_id": "launch:replacement",
  "acknowledged_at": "2026-11-19T18:00:00.000000001Z"
}
```

Migration 0110 records explicit `continuity=relaunch` provenance in the same
transaction as an ordinary worker session create. Source/replacement IDs,
logical agent and both placements come from daemon rows. An empty source worker
means the source was local; `worker_id` must be nonempty. Native-only and team
recovery do not synthesize this ordinary-create provenance.

Only the hub's authenticated `launch:<replacement>` result path writes the
immutable receipt, after checking exact `launched` type, session and command
identity with no worker error. Fresh/generic resume rows, caller JSON, native
mapping, state frames, GET, reconnect and an unknown/timed-out/lost response
cannot create it. A retry preserves the original acknowledgement timestamp;
receipt identity cannot be updated or copied to another attempt. The getter
rechecks the immutable provenance against current canonical row identities and
placements, plus parent/resume. Only settled sources and running or settled
historical destinations are projected; orphaned does not count as settled for
ordinary worker lineage. Missing historical acknowledgements stay null.

There is deliberately no provider thread, generation, workroot, token, raw
command result, custody, protocol payload or binding secret in this receipt.
An acknowledgement is a historical positive verdict, not proof of current
liveness. Consumers must fetch/validate canonical rows and current tip state.
Read validation and the consumer's local transaction cannot be globally atomic;
these facts are verified at observation time and authorize no runtime action.

## Validation

`internal/store/ui_lineage_test.go` covers explicit provenance, exact verdict,
state-only and generic-resume refusal, identity/placement mismatch, persistence,
immutable replay, copied receipts and no native promotion.
`internal/app/ui_lineage_test.go` uses actual retained replacement transactions
for A→B→C, independent actor checks and orphaned-source qualification, plus fake
authenticated worker command replies for ordinary success, wrong verdicts, rejection and unknown-outcome followed by a
late state frame.
`cmd/tether/ui_lineage_test.go` verifies the production authenticated item route,
remote reads, null historical/collection detail and native separation.
`internal/api/ui_lineage_test.go` covers replacement projection and explicit
failure without exposing private error payloads. Tachyon reconciliation and
native-POST rejection after a UI receipt are consumer-owned tests.

## Ordinary hub orphan recovery

`hub_resume_lineage` is null unless persisted ordinary hub resume plans establish
this exact UI ancestry:

```json
{"kind":"hub_resume_v1","source_session_id":"source","replacement_session_id":"replacement","logical_agent_id":"same-seat"}
```

Both canonical rows must be local and name the same nonempty logical agent; the
replacement has explicit resume intent and its exact source parent. Both stored
plans must name that agent, neither can be a team plan, and the replacement plan
must explicitly name `ResumeSourceSessionID`. Retained source shim custody or an
uncertain custody read refuses evidence. A lineage-only create with no stored
resume source does not gain this proof.

This covers existing successful ordinary hub recovery from a positively gone
orphan, whose source row deliberately remains orphaned. Historical destinations
may later be settled or orphaned; the UI separately requires its current tip to
be running. Detached, unknown or active sources are not admitted. This is stored
UI ancestry, with no fabricated acknowledgement time and no native admission.
