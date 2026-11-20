# Session output retention and summaries

Policy proposal for CW-20261120-0005. This document records the policy; it does
not enable a new sweeper, change catalog settings, reclassify historical rows,
or delete data. The correlation backfill is a separate metadata-only operation.

## Existing behavior

`session.output` is raw provider output and can contain model text, tool inputs,
and tool results. At insertion, the existing retention admission classifies it
as `R-debug`. The daemon's existing hourly sweep uses
`daemon.events_retention.debug_days` (default 7) for that class and the general
`days` window (default 90) for event history. The debug pass runs only when its
window is positive and shorter than the general window; the general pass also
covers debug rows. Thus `debug_days` does not extend the general window.
`enabled: false` or general `days < 1` disables this sweep, including its other
histories. These settings are global retention controls, not an output-only
switch.

Historical output rows without a retention class remain on the general window.
The stamping backfill must not set their retention class or timestamps. Ordinary
event summaries live on their source row and expire with it; they are not a
separate durable summary archive. The existing post-delete `RetentionSweep`
hook reports committed deletions; it cannot recover removed bodies and must
not be used as an archive-before-delete mechanism.

## Proposed policy

Keep the existing seven-day diagnostic window for newly classified raw output
and the 90-day window for lifecycle and useful structured activity. Raw output
is diagnostic evidence, not the status feed or the permanent transcript store.
A longer retention obligation must be met by a separately verified transcript
archive; the presence of a session log alone does not prove archive coverage.
Do not shorten historical output retention or bulk-reclassify it until the
summary/archive workflow below is implemented and verified on a database copy.

For a future implementation, produce a deterministic summary per canonical
session and UTC day. Use bounded pages over a fixed event sequence range.
Record the summary version, session ID, source range and cutoff, output row
count, payload byte count, first/last output times, and explicit completeness
state. Include task, run, workstream and provider session identifiers only when
unambiguous; if attribution changes within a window, retain distinct counts per
attribution tuple rather than labeling the whole window with the latest value.
The summary must not contain raw output excerpts, prompts, tool arguments or
results. A byte count measures the stored payload bytes, not model token usage.

Persist summaries separately from raw events with a unique key covering the
session, UTC day, summary version and source range. Retries must be idempotent.
Late output requires a new version or an explicitly extended range; never mark
an incomplete window complete. Keep completed summaries for 90 days from the
end of their covered day. Retain lifecycle events on their existing policy.
Summary expiry itself needs an explicit implementation and review; this
proposal schedules neither summary expiry nor raw-output deletion.

Any future summary-dependent cleanup must be opt-in and disabled by default,
with a report-only mode. Before each bounded deletion batch it must verify a
committed summary covers exactly that range and, where transcript preservation
is required, a readable archive receipt with matching counts/digests. Missing,
failed or incomplete coverage holds the batch. Preserve sequence-gap/replay
semantics and the existing durable retention receipts. Exclude active sessions
and windows still accepting output from this new cleanup path. This is a future
requirement, not a claim that the current age-based sweeper has these guards.

## Rollout and verification

1. Implement summary production/reporting with deletion disabled; measure
   coverage and resource costs against a database copy.
2. Verify retries, late output, changed attribution, incomplete archive writes,
   active sessions, and exact boundary timestamps. Report counts and bytes,
   without raw payloads.
3. Before enabling summary-dependent cleanup, reconcile it with the existing
   age-based sweep so that the old path cannot bypass the coverage guard. Review
   a backup/restore drill and explicit configuration in a separate unit.
4. Enable only through that later rollout. Turning off the new cleanup flag
   stops future deletions; restoring an already removed body requires the
   verified backup/archive. No flag can undo deletion.

CW-20261120-0005 performs no live database backfill, retention sweep, deletion,
configuration change, release or deployment.

References: [Event vocabulary](event-vocabulary.md#retention),
[Retention settings](agents/mcp/budgets.md#event-retention),
`internal/app/events_retention.go`, `internal/store/event_retention.go`.
