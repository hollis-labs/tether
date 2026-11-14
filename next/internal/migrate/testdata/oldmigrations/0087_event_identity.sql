-- CW-20261016-0136 X1d: storage for the write-once environment identity and
-- cutover. Startup seeds this before accepting writes. No index or backfill.
CREATE TABLE event_identity (
 singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
 environment_id TEXT NOT NULL,
 cutover INTEGER NOT NULL CHECK (cutover >= 0),
 next_legacy_id INTEGER NOT NULL CHECK (next_legacy_id >= 0),
 remaining INTEGER NOT NULL CHECK (remaining >= 0),
 backfilled INTEGER NOT NULL DEFAULT 0 CHECK (backfilled >= 0),
 failed INTEGER NOT NULL DEFAULT 0 CHECK (failed >= 0)
);
