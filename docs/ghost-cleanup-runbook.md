# Ghost cleanup runbook

`tether sessions reap-ghosts` (CW-20261114-0007) finds rows that claim a
session, a shim or a worker is alive when it is not, and cleans them up. It runs
on the hub host through the daemon (`POST /ghosts/reap`, admin scope), because
only the running hub knows each connected worker's live inventory.

## What it finds

| Kind | Class | Rule | Apply |
|---|---|---|---|
| Session | `worker_missing` / `worker_exited` | an open row (launching, running, detached, idle, orphaned) on a **connected** worker whose live inventory does not hold it, or holds it as exited, created more than 10 minutes ago | close |
| Session | (same, blocked) | as above, but the worker's hello does not prove its inventory complete: it predates the restore counts of CW-20261114-0056, or a restore failed | refused |
| Session | `worker_gone` | an open row on a worker that is not connected and not seen for 7 days | close |
| Session | `hub_orphaned` | a hub-local row the session reaper marked `orphaned`, with no managed runtime, no live pid (same pid identity check as the reaper) and no live shim | close |
| Custody | dead | a `session_shims` row of an ended session with no live host, shim or provider pid and no control socket; custody frozen by a session replacement is kept | delete |
| Worker | stale | a worker row not connected and not seen for 7 days with no open session (after this run's closes) | retire |

**Close** sets the session `failed`, revokes its session credential and runtime
bindings, and publishes a `session.reaper` event (`reason: ghost:<class>`,
`outcome: failed`) in the same transaction; then, as a normal session end does,
it deny-lists the session's agent tokens. Each write re-checks the row it was
classified from: a session that changed since is left alone.

**Retire** sets `workers.retired_at` and `superseded_by` (the newest live row of
the same label, if any). A retired row is hidden from `tether workers` and from
label lookups, still resolves by its exact id, and is un-retired by a hello from
its device. Workers are never deleted: ended sessions, mail grants and events
still name them, and proving nothing does would need a scan of the unindexed
events table, which holds the hub's single database connection.

Hub-local `launching`/`running`/`detached` rows are not in scope: the session
reaper already orphans them. Worker-side host directories are pruned by the
worker itself (CW-20261114-0056).

## Procedure

1. Dry run (changes nothing):

   ```sh
   tether sessions reap-ghosts            # or --json
   ```

   Review every line. Blocked sessions sit on a worker that has not been
   upgraded to a release with restore counts; upgrade it, let it reconnect, and
   run again.

2. Back up the database with SQLite's online backup (safe while the hub runs):

   ```sh
   dir=/srv/data/backups/pre-change/$(date -u +%Y%m%d)-ghost-cleanup
   mkdir -p "$dir"
   sqlite3 /var/lib/fabric/tether/state/tether.db ".backup '$dir/tether.db'"
   ```

3. Apply, naming the backup. It is refused unless the file exists, was written
   within the last 2 hours and starts with the SQLite header (no integrity
   check is run on it):

   ```sh
   tether sessions reap-ghosts --apply --backup "$dir/tether.db"
   ```

   The output shows what was done (`(done)`), and `before:` and `after:`
   counts. After a clean apply, `after:` shows no ghost sessions except blocked
   ones, no dead custody and no stale workers.

4. Record the before and after counts and the backup path on the task.

To undo, stop the hub and restore the backup; there is no per-row undo.
