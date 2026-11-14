-- 0016_group_messaging.sql
--
-- Sprint v060-05 (Group Messaging). Adds the `group` kind to the registry,
-- a `group_members` sibling table, and group columns on `messages`.
--
-- Three changes:
--
--   1. Extend `registry_entries.kind` CHECK from ('agent','project') to
--      ('agent','project','group'). SQLite has no ALTER COLUMN CHECK, so
--      this uses the canonical table-swap dance (CREATE new, INSERT FROM,
--      DROP old, RENAME). Indexes are recreated.
--
--   2. Add `group_members` (D5 sibling table — membership carries
--      per-member state {role, joined_at, last_read_seq}, so it can't live
--      in registry_links).
--
--   3. Add `messages.group_urn` + `messages.group_seq` + index for
--      group-scoped reads (D4 mailbox-pull — one row per message, not N).
--
-- FK declarations follow ADR-0008 (deferred enforcement) — declarations
-- live as documentation, PRAGMA foreign_keys stays off. v060-02 T-08 will
-- audit and flip enforcement globally, paired with the cerberus-bootstrap
-- workload that stress-tests cross-substrate FK paths.

-- 1. Extend registry_entries.kind CHECK to include 'group'.
CREATE TABLE registry_entries_new (
    urn              TEXT NOT NULL PRIMARY KEY,
    kind             TEXT NOT NULL CHECK(kind IN ('agent', 'project', 'group')),
    mux_instance_id  TEXT NOT NULL DEFAULT 'agent-mux',
    display_name     TEXT NOT NULL,
    title            TEXT,
    role             TEXT,
    description      TEXT,
    avatar           TEXT,
    project          TEXT,
    status           TEXT NOT NULL DEFAULT 'active',
    callback_json    TEXT,
    cached_at        DATETIME,
    health_status    TEXT,
    last_seen_at     DATETIME,
    host_address     TEXT,
    kind_meta_json   TEXT,
    last_updated_by  TEXT,
    created_at       DATETIME NOT NULL,
    updated_at       DATETIME NOT NULL
);

INSERT INTO registry_entries_new
SELECT urn, kind, mux_instance_id, display_name, title, role, description,
       avatar, project, status, callback_json, cached_at, health_status,
       last_seen_at, host_address, kind_meta_json, last_updated_by,
       created_at, updated_at
FROM registry_entries;

DROP TABLE registry_entries;
ALTER TABLE registry_entries_new RENAME TO registry_entries;

CREATE INDEX IF NOT EXISTS idx_registry_entries_kind
    ON registry_entries(kind);
CREATE INDEX IF NOT EXISTS idx_registry_entries_kind_status
    ON registry_entries(kind, status);
CREATE INDEX IF NOT EXISTS idx_registry_entries_kind_project
    ON registry_entries(kind, project);
CREATE INDEX IF NOT EXISTS idx_registry_entries_kind_role
    ON registry_entries(kind, role);

-- 2. group_members sibling table (D5).
CREATE TABLE group_members (
    grp_urn       TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    member_urn    TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    role          TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('member','moderator','owner')),
    joined_at     DATETIME NOT NULL,
    last_read_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (grp_urn, member_urn)
);

CREATE INDEX IF NOT EXISTS idx_group_members_by_member
    ON group_members(member_urn);

-- 3. messages.group_urn + group_seq columns + index for group-scoped reads.
-- ON DELETE SET NULL preserves message history when a group is hard-deleted
-- (v1 only soft-deletes per D9, but the FK semantic is here for the v060-02
-- enforcement flip). group_seq is a per-group monotonic counter assigned at
-- send time (T-04 implementation detail).
ALTER TABLE messages ADD COLUMN group_urn TEXT REFERENCES registry_entries(urn) ON DELETE SET NULL;
ALTER TABLE messages ADD COLUMN group_seq INTEGER;

CREATE INDEX IF NOT EXISTS idx_messages_group_seq
    ON messages(group_urn, group_seq);
