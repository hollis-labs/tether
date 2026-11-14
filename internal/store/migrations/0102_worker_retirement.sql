-- CW-20261114-0007: a worker row superseded by a newer enrollment of the same
-- label is retired, not deleted: ended sessions, mail grants and events still
-- name its id. A retired row is hidden from listings and label lookups; a
-- hello from its device un-retires it.
ALTER TABLE workers ADD COLUMN retired_at TEXT;
ALTER TABLE workers ADD COLUMN superseded_by TEXT;
