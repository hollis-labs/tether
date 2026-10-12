-- Forums are hosted separately from messages and registry operational content.
CREATE TABLE forums (
 id TEXT PRIMARY KEY, namespace TEXT NOT NULL UNIQUE, title TEXT NOT NULL,
 owner_type TEXT NOT NULL, owner_id TEXT NOT NULL,
 config_json TEXT NOT NULL DEFAULT '{}', policy_json TEXT NOT NULL DEFAULT '{"mode":"owner-only"}', metadata_json TEXT NOT NULL DEFAULT '{}',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE forum_topics (
 id TEXT PRIMARY KEY, forum_id TEXT NOT NULL REFERENCES forums(id), title TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','resolved','archived')),
 author_id TEXT NOT NULL, author_kind TEXT NOT NULL,
 post_count INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, last_activity_at TEXT NOT NULL, last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX forum_topics_scope ON forum_topics(forum_id,id);
CREATE TABLE forum_posts (
 id TEXT PRIMARY KEY, forum_id TEXT NOT NULL REFERENCES forums(id), topic_id TEXT NOT NULL REFERENCES forum_topics(id),
 parent_post_id TEXT NOT NULL DEFAULT '', seq INTEGER NOT NULL, body_md TEXT NOT NULL, body_chars INTEGER NOT NULL,
 author_id TEXT NOT NULL, author_kind TEXT NOT NULL, verified INTEGER NOT NULL,
 session_id TEXT NOT NULL DEFAULT '', agent_urn TEXT NOT NULL DEFAULT '', workstream_id TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '', launch_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, edited_at TEXT NOT NULL DEFAULT '', depth INTEGER NOT NULL DEFAULT 0, reply_count INTEGER NOT NULL DEFAULT 0,
 UNIQUE(forum_id,seq)
);
CREATE INDEX forum_posts_topic_seq ON forum_posts(topic_id,seq);
CREATE INDEX forum_posts_parent ON forum_posts(parent_post_id);
CREATE INDEX forum_posts_workstream ON forum_posts(forum_id,workstream_id,seq);
CREATE INDEX forum_posts_project ON forum_posts(forum_id,project_id,seq);
CREATE VIRTUAL TABLE forum_post_fts USING fts5(forum_id UNINDEXED, post_id UNINDEXED, topic_title, body_md);
-- post_ref, post_revision, post_state and fork_origin are separate future tables;
-- they do not overload messages or require changes to the identities above.
