# Event stages and source acceptance

Tether uses harness/core v0.13.0 `eventstage.Chain` with security/redact v0.2.0 policies.
The sanitizer release is v0.2.0; the similarly named security/secrets/redact package
is a separate credential-text redactor. Hadron PR36 (merged 80ed8a5) is the chain
reference; Tether adapts only kind, payload, digest and class, preserving its local
envelope and consumers.

`daemon.event_stage_mode` in global.yaml is read at startup:

```yaml
daemon:
  event_stage_mode: shadow
```

Omitted means `shadow`. `enforce` is an explicit source option, never enabled by a
library/schema upgrade. Invalid values fail startup. This change does not deploy
or activate enforcement. Rollback is the previous source/release; selecting shadow
at the next startup preserves local payload/class behavior.

Both Publish and PublishTx run stamp -> sanitize -> classify -> persist. Existing
stamp stages retain their order, per-stage deep clone/error isolation and TxSafe
selection: row lookups cannot run while the caller holds the database connection.
Sanitize/classify use only CPU and run in caller transactions. Persisted events
reach subscribers before Publish observers; PublishTx observers retain their prior
behavior. Sanitation failure is distinct from stamp failure: in enforce mode it
keeps the chain's safe error payload instead of restoring raw bytes.

The explicit registry in internal/events/pipeline_registry.go defines three classes:

- Metadata (A): usage, health, canonical tool start/finish, stalled, machine
  authority decisions, broker envelope metadata, daemon start/shutdown boundaries,
  atomic turn-routing audit, and typed X2 wake journal records. Policies list payload-struct paths; unknown fields are
  removed, credential/PII values masked, and enforce marks successful sanitation.
- Compatibility (B): other registered local payloads, including turn_output,
  mixed lifecycle, legacy tool calls and message delivery records. Payload bytes remain
  unchanged in either mode. Enforce marks them unsanitized; this slice does not
  close their raw-ingress/export sanitation policy or migrate their consumers.
- Unregistered (C): shadow keeps the original; enforce emits the shared chain's
  `{"sanitize_error":"unknown_kind"}` payload with sanitized=false.

Shadow runs sanitation/classification on a detached copy and returns only the real
stamp result to local persistence. It does not change payload/class, downgrade
an independently proven existing sanitation guarantee, or claim new enforcement.
The same code builds the candidate for enforce. Source kind aliases whose wire
values match the catalog share the policy automatically. Legacy tool-call records
have different payload schemas and remain B, not aliases of canonical tool events.

Sensitivity and PII floors are never lowered; existing retention admission still
rejects secret-class events. A sanitized candidate is not a bypass of that policy.
Producer digest plus payload fails closed using harness v0.13.0 tombstone handling.
Genuine payload-free retention tombstones remain unchanged, including for unknown
kinds. Existing failed-closed payloads remain unsanitized rather than being re-trusted.
Classification reads only successfully sanitized A candidates, never B text.
Unknown values are not used to infer scope or actor. Caller resolution requires
an existing verified principal before selecting session facts; missing bindings
and unmanaged sessions remain unresolved.

`Service.EventPipeline.Counts()` exposes detached, value-free per-kind counts for
candidate payload changes, redactions and failures. All unknown kind strings share
one bounded bucket. Error-class keys are closed; no payloads, input field paths or
raw errors are recorded. B counters describe candidate sanitation only. No live
payload collection, rollout sampling, deployment or enforcement activation is
claimed by fixture evidence.

Source checks include metadata JSON-equal goldens, compatibility byte equality,
secret/PII masking, unknown/error/panic/oversize/tombstone handling, transaction
parity and rollback, existing observer/clone ordering, and real service/registry
managed/unmanaged persisted/live attribution. The new AST source guard checks
local event constants, direct Event.Kind expressions, catalog kind references and
publishSessionEvent literals. It complements runtime fixtures; dynamic third-party
kinds remain C by absence. Reviewer acceptance of this new guard is required as
part of the normal review, not an assertion that static scanning proves all runtime
producers. Owner live sampling/activation/rollback evidence remains separate.
