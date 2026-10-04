# Inert team host

This package adapts the tagged mesh team contracts to durable SQLite state. It
is not registered with the daemon and starts no background work. Its database
handle and `teamstore.Store` must share the same migrated database handle.
Construction registers a launch-write callback on that store before launch use.
`PutLaunch` calls it with the normalized record on the same connection and under
its lease transaction, so lookup failure rolls back the launch and journal too.
Identical retries can restore a missing lookup without adding journal entries.
Plain teamstore writers work without the callback; migration backfill covers
legacy launch records.

The four ports are session lifecycle, enrollment/binding ownership, retained
message delivery and channel naming. The later activation adapter must prove
principal identity before reaching these contracts; actor identifiers alone are
not authentication. Team definitions must use strict authority. Trust policy is
explicit by definition pin or authenticated actor; omitted classifications deny.

Port idempotency must survive lost acknowledgements. Session stop, enrollment
release/retirement and binding release must durably fence an intent key, including
when cleanup sees a stub with no actor. They clean only resources acquired by
that key. A sessionless governance owner without a host acquisition keeps its
external resources when the run ends; a host acquisition missing its intent is
refused. The host persists the tombstone before calling them. Atomic, exclusive
binding ownership at the enroller spans all users of that registry, not just this
package's own binding table. Pool and durable identities must already be enrolled;
fresh enrollment is deterministic per intent and disappears on retirement.

Workflow creation and failure share transactions with the run container. Library
channel metadata stays `team/<run>`, while the session group and transport name
are derived as `team.<run>`. Channel naming creates no channel or content.

`SendMessage` accepts a durable outbox queue entry and binds its full retained
plan. New replies recheck delegation state and both member/session pairs in the
same transaction as acceptance; terminal delegations refuse new results.
Accepted receipts replay before liveness checks. `FlushMessages` dispatches the
queue with the same key, content, recipient session and delivery policy; failures
are returned and retained for retry. A successful transport acknowledgement is
recorded separately, so a crash retries the same receipt. Recovery pages follow
insertion sequence and retain rotating cursors across reopen; a failed entry
cannot starve unrelated recipients. A pending or backing-off head blocks later
messages for its retained actor/session pair; dead letters unblock that pair.
Attempts and last errors are durable. Retryable failures back off from one second
up to one minute, without an attempt limit. Explicit `ErrInvalidRequest` causes
operator-visible dead letters; `ErrSessionGone` dead-letters deliveries, while
`ErrSessionUnavailable` (including detached sessions) and ordinary errors retry.
Cleanup treats Stop `ErrNotFound`/`ErrSessionGone` as already gone, and preserves
binding/enrollment on other Stop failures. Dead-lettered delegates become failed
in the same transaction. `ListDeadLetters` exposes retained errors and attempts;
`ReviveDeadLetter` explicitly clears a row's dead state/backoff without retargeting
or reopening terminal delegation outcomes. Cleanup preserves resource ownership
until Stop succeeds. The messenger must
refuse unavailable sessions and unsupported policies rather than retarget or
inject an at-idle reply immediately. Activation must schedule this flush and
`ReconcileIntents`; construction does neither. Port errors are not hidden by
those recovery methods. Queues, receipts, tombstones and completed calls are
retained until an explicit host retention policy is introduced.

`Host.GetRun` returns library run metadata. `Calls` retains caller-scoped intent,
plan and completion bytes under the matching `ServiceKey` ledger lease. Request,
plan and result bytes are first-wins, detached and immutable. All related host
writes compose through teamstore's transaction fence. Provision validates identity
against the authored pool and any run-specific PoolIdentities subset. The library
is trusted to authorize requests and retain the rest of each slot plan; the host
checks slot name, definition and resolution, rather than independently rerunning
all spawn admission policy.

Recovery commits its cursor before calling ports. If processing is interrupted,
that page becomes eligible again after the cursor wraps. The activation driver
should serialize recovery: simultaneous flushers may call the same idempotent
port receipt and count two attempts. Ports must retain deduplication receipts.
Sequence columns use INTEGER PRIMARY KEY without AUTOINCREMENT because rows are
currently retained. A future retention implementation must preserve a durable
sequence high-water mark (or introduce AUTOINCREMENT) before deleting highest
rows; reusing sequence numbers would invalidate ordering/cursor assumptions.
