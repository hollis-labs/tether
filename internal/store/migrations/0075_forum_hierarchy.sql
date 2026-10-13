-- Forums hierarchy: remove namespace uniqueness while preserving existing rows.
CREATE TEMP TABLE saved_forums AS SELECT * FROM forums;
CREATE TEMP TABLE saved_forum_topics AS SELECT * FROM forum_topics;
CREATE TEMP TABLE saved_forum_posts AS SELECT * FROM forum_posts;
CREATE TEMP TABLE saved_forum_participants AS SELECT * FROM forum_participants;
DROP TABLE forum_post_fts;
DROP TABLE forum_participants;
DROP TABLE forum_posts;
DROP TABLE forum_topics;
DROP TABLE forums;
CREATE TABLE forum_categories (id TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', archived INTEGER NOT NULL DEFAULT 0, owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX forum_categories_namespace ON forum_categories(namespace,id);
-- Forums are hosted separately from messages and registry operational content.
CREATE TABLE forums (
 category_id TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', archived INTEGER NOT NULL DEFAULT 0,
 id TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL,
 owner_type TEXT NOT NULL, owner_id TEXT NOT NULL,
 config_json TEXT NOT NULL DEFAULT '{}', policy_json TEXT NOT NULL DEFAULT '{"mode":"owner-only"}', metadata_json TEXT NOT NULL DEFAULT '{}',
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE forum_topics (
 source_topic_id TEXT NOT NULL DEFAULT '', archived INTEGER NOT NULL DEFAULT 0,
 id TEXT PRIMARY KEY, forum_id TEXT NOT NULL REFERENCES forums(id), title TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','resolved','archived')),
 author_id TEXT NOT NULL, author_kind TEXT NOT NULL,
 post_count INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, last_activity_at TEXT NOT NULL, last_seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX forum_topics_scope ON forum_topics(forum_id,id);
CREATE TABLE forum_posts (
 subject TEXT NOT NULL DEFAULT '', attachments_json TEXT NOT NULL DEFAULT '[]', source_post_id TEXT NOT NULL DEFAULT '',
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
-- Existing owners and operator access remain unchanged; no automatic invitations.
CREATE TABLE forum_participants (
 forum_id TEXT NOT NULL REFERENCES forums(id),
 principal_kind TEXT NOT NULL CHECK(principal_kind IN ('user','agent')),
 principal_id TEXT NOT NULL,
 access TEXT NOT NULL CHECK(access IN ('read','post')),
 PRIMARY KEY (forum_id,principal_kind,principal_id)
);
CREATE INDEX forum_participants_principal ON forum_participants(principal_kind,principal_id,forum_id);
INSERT INTO forums (id,namespace,title,owner_type,owner_id,config_json,policy_json,metadata_json,created_at,updated_at,last_seq) SELECT id,namespace,title,owner_type,owner_id,config_json,policy_json,metadata_json,created_at,updated_at,last_seq FROM saved_forums;
DROP TABLE saved_forums;
INSERT INTO forum_topics (id,forum_id,title,status,author_id,author_kind,post_count,created_at,last_activity_at,last_seq) SELECT id,forum_id,title,status,author_id,author_kind,post_count,created_at,last_activity_at,last_seq FROM saved_forum_topics;
DROP TABLE saved_forum_topics;
INSERT INTO forum_posts (id,forum_id,topic_id,parent_post_id,seq,body_md,body_chars,author_id,author_kind,verified,session_id,agent_urn,workstream_id,project_id,launch_id,created_at,edited_at,depth,reply_count) SELECT id,forum_id,topic_id,parent_post_id,seq,body_md,body_chars,author_id,author_kind,verified,session_id,agent_urn,workstream_id,project_id,launch_id,created_at,edited_at,depth,reply_count FROM saved_forum_posts;
DROP TABLE saved_forum_posts;
INSERT INTO forum_participants (forum_id,principal_kind,principal_id,access) SELECT forum_id,principal_kind,principal_id,access FROM saved_forum_participants;
DROP TABLE saved_forum_participants;
INSERT INTO forum_post_fts (forum_id,post_id,topic_title,body_md) SELECT p.forum_id,p.id,t.title,p.body_md FROM forum_posts p JOIN forum_topics t ON t.id=p.topic_id;
CREATE INDEX forums_category ON forums(namespace,category_id,id);

UPDATE forum_topics SET archived=1,status='open' WHERE status='archived';
