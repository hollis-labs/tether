-- Conversation-to-session mapping (CW-20261118-0008, docs/conversations.md).
--
-- One row per conversation: "conversation C, at generation N, runs in session
-- S (attempt A, worker W)". The daemon mints conversation_id (an opaque ULID,
-- never derived from a session id) when the first session for a seat is
-- created, and moves the row to each later session of that seat in the same
-- transaction as the session write, raising generation by exactly 1.
--
-- IF NOT EXISTS keeps the file safe to re-run by hand; the migrator already
-- applies it once. No backfill: an existing seat gets its row on its next
-- session create, relaunch or replacement.
CREATE TABLE IF NOT EXISTS conversations (
    conversation_id TEXT PRIMARY KEY NOT NULL,
    owner           TEXT NOT NULL,
    agent           TEXT NOT NULL,
    seat            TEXT NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'stable' CHECK (kind IN ('stable', 'embedded')),
    generation      INTEGER NOT NULL CHECK (generation >= 1),
    session_id      TEXT NOT NULL,
    attempt_id      TEXT NOT NULL,
    worker_id       TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_conversations_stable_seat
    ON conversations(owner, agent, seat) WHERE kind = 'stable';
CREATE INDEX IF NOT EXISTS idx_conversations_session ON conversations(session_id);

-- Every generation a conversation has had, so a superseded session still
-- resolves to its conversation (and can be told it is stale). Append-only.
CREATE TABLE IF NOT EXISTS conversation_generations (
    conversation_id TEXT NOT NULL,
    generation      INTEGER NOT NULL,
    session_id      TEXT NOT NULL,
    attempt_id      TEXT NOT NULL,
    worker_id       TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL CHECK (reason IN ('create', 'launch', 'resume', 'replace')),
    created_at      TEXT NOT NULL,
    PRIMARY KEY (conversation_id, generation)
);
CREATE INDEX IF NOT EXISTS idx_conversation_generations_session
    ON conversation_generations(session_id);
