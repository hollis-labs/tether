-- CW-20261016-0157: source is daemon-derived, not caller-asserted.
-- Every prior proxy row could be written through the public refs endpoint;
-- none qualifies for the historical daemon-only exception. Preserve all other
-- row identity, relation, timestamp and parent data. Forward-only: old binaries
-- must not be used against this expanded source vocabulary after migration.
CREATE TABLE session_refs_verified (
    id INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    ref_id TEXT NOT NULL,
    uri TEXT,
    relation TEXT NOT NULL CHECK(relation IN ('created', 'updated', 'read', 'referenced')),
    source TEXT NOT NULL CHECK(source IN ('proxy', 'proxy_claimed', 'session', 'operator', 'api', 'agent')),
    at TEXT NOT NULL,
    parent_item_id TEXT,
    UNIQUE(session_id, kind, ref_id, relation)
);
INSERT INTO session_refs_verified (id, session_id, kind, ref_id, uri, relation, source, at, parent_item_id)
SELECT id, session_id, kind, ref_id, uri, relation,
       CASE source WHEN 'proxy' THEN 'proxy_claimed' ELSE source END,
       at, parent_item_id
FROM session_refs;
DROP TABLE session_refs;
ALTER TABLE session_refs_verified RENAME TO session_refs;
CREATE INDEX idx_session_refs_session ON session_refs(session_id);
CREATE INDEX idx_session_refs_lookup ON session_refs(kind, ref_id);
CREATE INDEX idx_session_refs_parent_item ON session_refs(parent_item_id);
