# Event session attribution backfill

The store fills missing task, workstream, run and native provider session IDs
from the persisted **subject** session. This covers background, legacy and
transactional writers. Reads use the writer's connection, including uncommitted
session data. Caller snapshots and payloads do not supply this lookup. Explicit
producer values are preserved; an event-ID retry returns the original row.
An unresolved caller snapshot suppresses all inferred attribution, matching the
existing eventstamp.Rows provenance boundary. Absent daemon snapshots and
resolved snapshots permit the subject-session lookup.

There is no `sessions.run_id`. Run attribution follows the unique
`sessions.session_group_id = team_runs.session_group_id` link. Sessions without
that link remain without a run. Native IDs come only from owner `tether`, using
the event's `process_provider` when present, otherwise `sessions.provider_id`.
An unmatched provider remains empty; other owners, parents and siblings are
never fallback sources.

## Copy-only historical verification

Requires Python 3.8+ with SQLite 3.33+ (`UPDATE FROM`). For this unit, use a
verified backup as the source; no live backfill or deletion is authorized.

```sh
python3 scripts/backfill-event-session-scopes.py \
  --source /path/to/verified-backup.db \
  --output /path/to/new-backfilled-copy.db \
  --batch-size 10000
```

The source opens read-only. SQLite online backup captures committed WAL data
in a consistent copy. Output creation is exclusive: existing files, symlinks
and hard links are refused. Only the new output is updated, in bounded batches.
Reserve space for another full DB copy. A failure may leave a partial output;
keep it for diagnosis and retry with a new output path. Never substitute a live
DB for the output.

The JSON report includes before/after total and populated counts for each
field, changed rows and `quick_check`. Divide populated counts by `rows` for
fill rates. Counts include all events, including unknown sessions. No payloads
or native IDs are printed. No rows are deleted or nonempty stamps overwritten.
Re-running on a backfilled copy into another new output changes zero rows
unless its session/mapping snapshot changed.

Historical attribution uses the copy's current session/mapping snapshot. The
mapping table does not retain previous native IDs, so the tool cannot recreate
older mapping versions or recover unrecorded task/run/workstream assignments.
It does not infer them from names, payloads or timestamps.

```sh
go test ./internal/store -run TestStoredSessionStamp -count=1
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts \
  -p backfill_event_session_scopes_test.py -v
```

The Python fixture verifies WAL capture, source immutability, sparse batch IDs,
owner/provider isolation, explicit values, unknown sessions, idempotence and
refusal to overwrite existing files.

For the documented output retention and summary proposal, see
[Session output retention and summaries](session-output-retention.md).
