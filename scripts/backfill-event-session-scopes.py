#!/usr/bin/env python3
"""Backfill event attribution into a NEW SQLite copy; never update the source."""

import argparse
import json
import os
from pathlib import Path
import sqlite3

FIELDS = ("scope_task_id", "scope_workstream_id", "scope_run_id", "provider_session_id")


def fill_rates(db):
    """Counts include all events; unknown sessions remain unfilled."""
    row = db.execute(
        "SELECT count(*), "
        + ", ".join(f"count(NULLIF({field},''))" for field in FIELDS)
        + " FROM events"
    ).fetchone()
    return {"rows": row[0], "filled": dict(zip(FIELDS, row[1:]))}


def backfill_copy(source, output, batch_size=10000):
    if batch_size < 1:
        raise ValueError("batch size must be positive")
    source = Path(source).resolve(strict=True)
    output = Path(output).absolute()
    # O_EXCL also refuses existing hard links/symlinks and prevents accidental
    # replacement of any existing DB. The only writable connection is this copy.
    fd = os.open(output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(fd)
    with sqlite3.connect(source.as_uri() + "?mode=ro", uri=True) as src:
        src.execute("PRAGMA query_only=ON")
        with sqlite3.connect(output) as dst:
            # SQLite's online backup includes committed WAL content and produces
            # a consistent snapshot without copying a live file behind SQLite.
            src.backup(dst)
            before = fill_rates(dst)
            high = dst.execute("SELECT COALESCE(max(id),0) FROM events").fetchone()[0]
            after_id = 0
            changed = 0
            while after_id < high:
                ids = dst.execute(
                    "SELECT id FROM events WHERE id>? AND id<=? ORDER BY id LIMIT ?",
                    (after_id, high, batch_size),
                ).fetchall()
                if not ids:
                    break
                end_id = ids[-1][0]
                # No payload parsing, fuzzy IDs, sibling/parent-session guesses,
                # or owner fallback. The session-group to team-run link is UNIQUE.
                dst.execute(
                    """WITH attribution AS (
                        SELECT e.id, s.task_id, s.workstream_id, r.run_id,
                               m.native_session_id
                        FROM events e JOIN sessions s ON s.id=e.session_id
                        LEFT JOIN team_runs r ON r.session_group_id=s.session_group_id
                        LEFT JOIN session_provider_mappings m
                          ON m.session_id=s.id AND m.owner='tether'
                         AND m.provider=COALESCE(NULLIF(e.process_provider,''),s.provider_id)
                        WHERE e.id>? AND e.id<=?
                    )
                    UPDATE events AS e SET
                        scope_task_id=COALESCE(NULLIF(e.scope_task_id,''),NULLIF(a.task_id,'')),
                        scope_workstream_id=COALESCE(NULLIF(e.scope_workstream_id,''),NULLIF(a.workstream_id,'')),
                        scope_run_id=COALESCE(NULLIF(e.scope_run_id,''),NULLIF(a.run_id,'')),
                        provider_session_id=COALESCE(NULLIF(e.provider_session_id,''),NULLIF(a.native_session_id,''))
                    FROM attribution a WHERE e.id=a.id AND (
                        (COALESCE(e.scope_task_id,'')='' AND COALESCE(a.task_id,'')!='') OR
                        (COALESCE(e.scope_workstream_id,'')='' AND COALESCE(a.workstream_id,'')!='') OR
                        (COALESCE(e.scope_run_id,'')='' AND COALESCE(a.run_id,'')!='') OR
                        (COALESCE(e.provider_session_id,'')='' AND COALESCE(a.native_session_id,'')!='')
                    )""",
                    (after_id, end_id),
                )
                changed += dst.execute("SELECT changes()").fetchone()[0]
                dst.commit()
                after_id = end_id
            after = fill_rates(dst)
            integrity = dst.execute("PRAGMA quick_check").fetchall()
            if integrity != [("ok",)]:
                raise RuntimeError(f"copy quick_check failed: {integrity}")
    # No row content, payloads, or native identifiers appear in the report.
    return {"source": str(source), "output": str(output), "before": before,
            "after": after, "changed_rows": changed, "quick_check": "ok"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, help="existing SQLite DB (opened read-only)")
    parser.add_argument("--output", required=True, help="NEW copy path; must not already exist")
    parser.add_argument("--batch-size", type=int, default=10000)
    args = parser.parse_args()
    print(json.dumps(backfill_copy(args.source, args.output, args.batch_size), indent=2))


if __name__ == "__main__":
    main()
