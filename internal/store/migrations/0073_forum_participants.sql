-- Existing owners and operator access remain unchanged; no automatic invitations.
CREATE TABLE forum_participants (
 forum_id TEXT NOT NULL REFERENCES forums(id),
 principal_kind TEXT NOT NULL CHECK(principal_kind IN ('user','agent')),
 principal_id TEXT NOT NULL,
 access TEXT NOT NULL CHECK(access IN ('read','post')),
 PRIMARY KEY (forum_id,principal_kind,principal_id)
);
CREATE INDEX forum_participants_principal ON forum_participants(principal_kind,principal_id,forum_id);
