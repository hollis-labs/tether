# Dormant event retention primitives

This package is preparation for D7 retention classes (CW-20261016-0146). It is
not connected to catalog loading, the daemon, or the current retention job.
The explicit store adapter is described below. The hub keeps its existing 90-day sweep until the approved log cutover.

`Parse` accepts a standalone proposed block, with strict unknown-field checks:

```yaml
retention:
  pii_cap: 30d
  acknowledge_risk: false
  classes:
    audit:
      full: 0
      envelope: none
      rollups: [{bucket: 1d, keep: 0}]
    state:
      full: 180d
      envelope: 2y
      rollups: [{bucket: 1h, keep: 2y}, {bucket: 1d, keep: 0}]
    activity:
      full: 30d
      envelope: 180d
      rollups: [{bucket: 1h, keep: 2y}, {bucket: 1d, keep: 0}]
    debug:
      full: 7d
      envelope: none
      rollups: [{bucket: 1h, keep: 90d}]
```

These are the defaults; omitted fields inherit them. `0` means forever, and
`none` disables only an envelope tier. Full/envelope are absolute age horizons
from server-recorded admission, not cumulative tier durations. A day is 24 hours
and a year 365 days; other positive Go durations are accepted. A class needs at
least one rollup bucket so its rows can eventually be trimmed safely. Buckets
must be positive and unique. Shortening retention is free, except that cutting
audit's full/envelope horizon below 400 days needs acknowledgment. Extending the
PII payload cap also needs acknowledgment. Both return warnings even with the
flag. Secrets cannot be configured for storage and secret records are rejected.

A compiled configuration is immutable and has a content-derived epoch ID.
`Snapshot` captures a class policy. Admission snapshots remain immutable audit
provenance, but each retention pass resolves every classified row against its
current configuration (D7 rev 2.2). Shortening reaches existing rows, including
PII; lengthening preserves remaining payloads but cannot restore tombstones.

`Plan` determines payload removal and trim eligibility. PII caps only payload
age: an audit event always keeps its envelope and row, even when its full
payload retention is finite. `Tombstone`
removes exact stored payload bytes and returns `sha256:<64 lowercase hex>`, or
`withheld` for PII (the amended digest ruling). It must only be called for an
eligible, present payload; nil means already absent and returns no new digest.
The adapter changes only payload and digest, preserving envelope, cursor, scope,
correlation, class, payload_schema, content_type and truncated. Tether's column
is the source of truth pending the additive wire field; the later mesh adapter
uses JSON null, while runtimeevents omits the payload.

`RollupBatch` returns deltas and a cursor checkpoint for an ordered stream. The
adapter must commit both in one transaction, then resume strictly after that
checkpoint; replaying committed cursors is a no-op. Buckets are UTC, half-open
intervals. Counts use class and PII status only, with first/last cursor. Epochs
stay separate so a new keep duration cannot shorten an older bucket. Rollup
expiry is measured from bucket end; zero keep never expires. Each configured
bucket is counted directly, avoiding double-counting hourly and daily totals.

`HeadTrim` requires rows from the actual log head in ascending cursor order,
never a prefiltered set of old rows. It stops at the first retained, pending,
legacy/unclassified or unrolled row; every audit row is a permanent barrier. It
cannot delete holes later in the stream. Numeric cursor gaps are allowed and
IDs are never reassigned. Pending outbox rows are also excluded from payload
removal and rollups.

Production integration must stack after X1a/X1c and consume persisted v1.1
class/sensitivity and recorded time. It must preserve unpublished outbox rows,
commit rollups before head deletion, use bounded transactions, preserve legacy
unclassified rows, and prevent the old age-only sweep from competing with class retention.
The separate identity_audit table retains its 400-day floor. None of that wiring
or a live retention-setting change is part of this package.

## Persistence preparation

The store adapter adds migration 0083 with immutable resolved policy snapshots
and an explicitly activated kind-to-class catalog. `SetEventRetentionAdmission`
replaces the catalog atomically; no daemon/catalog loader invokes it yet.
Snapshots survive later policy changes. Unknown kinds have NULL class/policy,
even if an event asserts them. The shared insert helper covers ordinary and
transactional outbox writes, rejects secret/unknown sensitivities, and uses the
caller's connection. Environment requests preserve their envelope through their
existing request-status transaction.

`TombstoneEventPayloads` scans at most 1,000 published, classified payloads per
transaction. Its caller must pass the current compiled configuration explicitly;
an uninitialized configuration is refused. It verifies admission snapshot
integrity for audit provenance, resolves eligibility from the current policy,
and clears only eligible payloads. Its conditional UPDATE repeats the pending/payload/policy
guards. A failure rolls back the whole batch. The result is a per-sweep scan
cursor, not a trim checkpoint: a later sweep starts again at zero to reconsider
younger or formerly pending rows. It changes neither row IDs nor envelope
metadata. There is no scheduler, row deletion, aggregate persistence or cutover
in this slice. Deployment/activation still requires disabling the competing
age-only event sweep and preserving the separate identity audit floor.

The local rollup primitives remain dormant preparation. The shared janitor and
its Store conformance suite will replace that integration surface after the
approved library release, including current-policy rollup expiry, per-class
removal watermarks and retention gap frames. This correction wires no scheduler
and does not change live settings.
