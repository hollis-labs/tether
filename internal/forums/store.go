// Package forums is Tether's host adapter for go-forums.
package forums

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	f "github.com/hollis-labs/go-forums"
	"github.com/hollis-labs/tether/internal/store"
)

type Store struct{ db *sql.DB }

func New(st *store.Store) *f.Service {
	s := &Store{db: st.DB()}
	return f.New(s, s, func() string { return uuid.Must(uuid.NewV7()).String() })
}
func mapErr(e error) error {
	if errors.Is(e, sql.ErrNoRows) {
		return f.ErrNotFound
	}
	if e != nil && strings.Contains(e.Error(), "UNIQUE constraint failed") {
		return f.ErrConflict
	}
	return e
}

type scanner interface{ Scan(...any) error }

const forumCols = `id,namespace,title,owner_type,owner_id,config_json,policy_json,metadata_json,created_at,updated_at,last_seq`

func scanForum(s scanner) (f.Forum, error) {
	var v f.Forum
	var c, p, m string
	e := s.Scan(&v.ID, &v.Namespace, &v.Title, &v.OwnerType, &v.OwnerID, &c, &p, &m, &v.CreatedAt, &v.UpdatedAt, &v.LastSeq)
	v.Config = json.RawMessage(c)
	v.Metadata = json.RawMessage(m)
	if e == nil {
		e = json.Unmarshal([]byte(p), &v.Policy)
	}
	return v, mapErr(e)
}
func (s *Store) GetForum(ctx context.Context, id string) (f.Forum, error) {
	return scanForum(s.db.QueryRowContext(ctx, `SELECT `+forumCols+` FROM forums WHERE id=?`, id))
}
func (s *Store) CreateForum(ctx context.Context, v f.Forum) (f.Forum, error) {
	p, _ := json.Marshal(v.Policy)
	_, e := s.db.ExecContext(ctx, `INSERT INTO forums (`+forumCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.Namespace, v.Title, v.OwnerType, v.OwnerID, string(v.Config), string(p), string(v.Metadata), v.CreatedAt, v.UpdatedAt, v.LastSeq)
	return v, mapErr(e)
}

// Cursors are bound to operation, scope and filters, so a token cannot silently
// skip data when reused on another forum or query. Limits may change between pages.
type cursor struct {
	Scope string `json:"s"`
	Key   string `json:"k"`
}

func scopeKey(scope string, o f.Options) string {
	o.Cursor = ""
	o.Limit = 0
	o.Detail = ""
	o.Fields = ""
	o.MaxChars = 0
	b, _ := json.Marshal(o)
	h := sha256.Sum256(append([]byte(scope), b...))
	return fmt.Sprintf("%x", h[:])
}
func decodeCursor(scope string, o f.Options) (string, error) {
	if o.Cursor == "" {
		return "", nil
	}
	b, e := base64.RawURLEncoding.DecodeString(o.Cursor)
	var c cursor
	if e != nil || json.Unmarshal(b, &c) != nil || c.Scope != scopeKey(scope, o) || c.Key == "" {
		return "", fmt.Errorf("%w: invalid cursor for these filters", f.ErrInvalid)
	}
	return c.Key, nil
}
func nextCursor(scope, key string, o f.Options) string {
	b, _ := json.Marshal(cursor{scopeKey(scope, o), key})
	return base64.RawURLEncoding.EncodeToString(b)
}
func (s *Store) ListForums(ctx context.Context, c f.Caller, o f.Options) (f.Page[f.Forum], error) {
	pg := f.Page[f.Forum]{Items: []f.Forum{}}
	scope := "forums:" + c.ID + ":" + c.Kind
	key, e := decodeCursor(scope, o)
	if e != nil {
		return pg, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT `+forumCols+` FROM forums WHERE id>? AND (? OR (owner_id=? AND owner_type=?)) ORDER BY id LIMIT ?`, key, c.Operator, c.ID, c.Kind, o.Limit+1)
	if e != nil {
		return pg, e
	}
	defer rows.Close()
	for rows.Next() {
		v, e := scanForum(rows)
		if e != nil {
			return pg, e
		}
		pg.Items = append(pg.Items, v)
	}
	if e = rows.Err(); e != nil {
		return pg, e
	}
	if len(pg.Items) > o.Limit {
		pg.HasMore = true
		pg.Items = pg.Items[:o.Limit]
		pg.NextCursor = nextCursor(scope, pg.Items[len(pg.Items)-1].ID, o)
	}
	return pg, nil
}

const topicCols = `id,forum_id,title,status,author_id,author_kind,post_count,created_at,last_activity_at,last_seq`

func scanTopic(s scanner) (f.Topic, error) {
	var v f.Topic
	e := s.Scan(&v.ID, &v.ForumID, &v.Title, &v.Status, &v.CreatedBy.ID, &v.CreatedBy.Kind, &v.PostCount, &v.CreatedAt, &v.LastActivityAt, &v.LastSeq)
	return v, mapErr(e)
}
func (s *Store) GetTopic(ctx context.Context, id string) (f.Topic, error) {
	return scanTopic(s.db.QueryRowContext(ctx, `SELECT `+topicCols+` FROM forum_topics WHERE id=?`, id))
}
func (s *Store) CreateTopic(ctx context.Context, v f.Topic) (f.Topic, error) {
	_, e := s.db.ExecContext(ctx, `INSERT INTO forum_topics (`+topicCols+`) SELECT ?,id,?,?,?,?,?,?,?,? FROM forums WHERE id=?`, v.ID, v.Title, v.Status, v.CreatedBy.ID, v.CreatedBy.Kind, v.PostCount, v.CreatedAt, v.LastActivityAt, v.LastSeq, v.ForumID)
	return v, mapErr(e)
}

const postCols = `p.id,p.topic_id,p.forum_id,p.parent_post_id,p.seq,p.body_md,p.body_chars,p.author_id,p.author_kind,p.verified,p.session_id,p.agent_urn,p.workstream_id,p.project_id,p.launch_id,p.created_at,p.edited_at,p.depth,p.reply_count,t.title`

func scanPost(s scanner) (f.Post, error) {
	var v f.Post
	e := s.Scan(&v.ID, &v.TopicID, &v.ForumID, &v.ParentPostID, &v.Seq, &v.BodyMD, &v.BodyChars, &v.Provenance.ID, &v.Provenance.Kind, &v.Provenance.Verified, &v.Provenance.SessionID, &v.Provenance.AgentURN, &v.Provenance.WorkstreamID, &v.Provenance.ProjectID, &v.Provenance.LaunchID, &v.CreatedAt, &v.EditedAt, &v.Depth, &v.ReplyCount, &v.TopicTitle)
	return v, mapErr(e)
}
func (s *Store) GetPost(ctx context.Context, id string) (f.Post, error) {
	return scanPost(s.db.QueryRowContext(ctx, `SELECT `+postCols+` FROM forum_posts p JOIN forum_topics t ON t.id=p.topic_id WHERE p.id=?`, id))
}
func (s *Store) CreatePost(ctx context.Context, v f.Post) (out f.Post, err error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return v, e
	}
	defer func() { _ = tx.Rollback() }()
	var title, status, forum string
	e = tx.QueryRowContext(ctx, `SELECT title,status,forum_id FROM forum_topics WHERE id=?`, v.TopicID).Scan(&title, &status, &forum)
	if e != nil {
		return v, mapErr(e)
	}
	if forum != v.ForumID {
		return v, f.ErrInvalid
	}
	if status != "open" {
		return v, f.ErrForbidden
	}
	if v.ParentPostID != "" {
		var topic string
		e = tx.QueryRowContext(ctx, `SELECT topic_id,depth FROM forum_posts WHERE id=?`, v.ParentPostID).Scan(&topic, &v.Depth)
		if e != nil {
			return v, mapErr(e)
		}
		if topic != v.TopicID {
			return v, fmt.Errorf("%w: parent_post_id must belong to this topic", f.ErrInvalid)
		}
		v.Depth++
		if v.Depth > 1000 {
			return v, fmt.Errorf("%w: thread depth exceeds 1000", f.ErrInvalid)
		}
	}
	e = tx.QueryRowContext(ctx, `UPDATE forums SET last_seq=last_seq+1,updated_at=? WHERE id=? RETURNING last_seq`, v.CreatedAt, v.ForumID).Scan(&v.Seq)
	if e != nil {
		return v, mapErr(e)
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO forum_posts (id,topic_id,forum_id,parent_post_id,seq,body_md,body_chars,author_id,author_kind,verified,session_id,agent_urn,workstream_id,project_id,launch_id,created_at,depth) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.TopicID, v.ForumID, v.ParentPostID, v.Seq, v.BodyMD, v.BodyChars, v.Provenance.ID, v.Provenance.Kind, v.Provenance.Verified, v.Provenance.SessionID, v.Provenance.AgentURN, v.Provenance.WorkstreamID, v.Provenance.ProjectID, v.Provenance.LaunchID, v.CreatedAt, v.Depth)
	if e != nil {
		return v, mapErr(e)
	}
	_, e = tx.ExecContext(ctx, `UPDATE forum_topics SET post_count=post_count+1,last_activity_at=?,last_seq=? WHERE id=?`, v.CreatedAt, v.Seq, v.TopicID)
	if e != nil {
		return v, e
	}
	if v.ParentPostID != "" {
		_, e = tx.ExecContext(ctx, `UPDATE forum_posts SET reply_count=reply_count+1 WHERE id=?`, v.ParentPostID)
		if e != nil {
			return v, e
		}
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO forum_post_fts (forum_id,post_id,topic_title,body_md) VALUES (?,?,?,?)`, v.ForumID, v.ID, title, v.BodyMD)
	if e != nil {
		return v, e
	}
	if v.Provenance.SessionID != "" {
		_, e = tx.ExecContext(ctx, `INSERT INTO session_refs (session_id,kind,ref_id,uri,relation,source,at) VALUES (?,?,?,?,?,?,?)`, v.Provenance.SessionID, store.KindForumPost, v.ID, "forum://"+v.ForumID+"/post/"+v.ID, store.RelationCreated, store.SourceAPI, v.CreatedAt)
		if e != nil {
			return v, e
		}
	}
	if e = tx.Commit(); e != nil {
		return v, e
	}
	v.TopicTitle = title
	return v, nil
}

// Filters are all bound parameters. Topic filters match post attributes within
// that topic, while author/date refer to the topic's own creation attributes.
func postFilters(o f.Options) (string, []any) {
	where := ` AND (?='' OR p.author_id=? OR p.agent_urn=?) AND (?='' OR p.author_kind=?) AND (?='' OR p.created_at>=?) AND (?='' OR p.created_at<=?) AND (?='' OR p.workstream_id=?) AND (?='' OR p.project_id=?) AND (?='' OR t.status=?)`
	args := []any{o.Author, o.Author, o.Author, o.AuthorKind, o.AuthorKind, o.CreatedAfter, o.CreatedAfter, o.CreatedBefore, o.CreatedBefore, o.WorkstreamID, o.WorkstreamID, o.ProjectID, o.ProjectID, o.Status, o.Status}
	if o.HasReplies != nil {
		where += ` AND (p.reply_count>0)=?`
		args = append(args, *o.HasReplies)
	}
	return where, args
}
func (s *Store) ListTopics(ctx context.Context, id string, o f.Options) (f.Page[f.Topic], error) {
	pg := f.Page[f.Topic]{Items: []f.Topic{}}
	scope := "topics:" + id
	key, e := decodeCursor(scope, o)
	if e != nil {
		return pg, e
	}
	where := ` WHERE forum_id=? AND id>? AND last_seq>=? AND (?='' OR status=?) AND (?='' OR author_id=?) AND (?='' OR author_kind=?) AND (?='' OR created_at>=?) AND (?='' OR created_at<=?)`
	args := []any{id, key, o.SinceSeq, o.Status, o.Status, o.Author, o.Author, o.AuthorKind, o.AuthorKind, o.CreatedAfter, o.CreatedAfter, o.CreatedBefore, o.CreatedBefore}
	if o.SinceSeq > 0 {
		where = strings.Replace(where, "last_seq>=?", "last_seq>?", 1)
	}
	if o.HasReplies != nil {
		where += ` AND EXISTS (SELECT 1 FROM forum_posts p WHERE p.topic_id=forum_topics.id AND p.parent_post_id<>'')=?`
		args = append(args, *o.HasReplies)
	}
	if o.WorkstreamID != "" || o.ProjectID != "" {
		where += ` AND EXISTS (SELECT 1 FROM forum_posts p WHERE p.topic_id=forum_topics.id AND (?='' OR p.workstream_id=?) AND (?='' OR p.project_id=?))`
		args = append(args, o.WorkstreamID, o.WorkstreamID, o.ProjectID, o.ProjectID)
	}
	args = append(args, o.Limit+1)
	rows, e := s.db.QueryContext(ctx, `SELECT `+topicCols+` FROM forum_topics`+where+` ORDER BY id LIMIT ?`, args...)
	if e != nil {
		return pg, e
	}
	defer rows.Close()
	for rows.Next() {
		v, e := scanTopic(rows)
		if e != nil {
			return pg, e
		}
		pg.Items = append(pg.Items, v)
	}
	if e = rows.Err(); e != nil {
		return pg, e
	}
	if len(pg.Items) > o.Limit {
		pg.HasMore = true
		pg.Items = pg.Items[:o.Limit]
		pg.NextCursor = nextCursor(scope, pg.Items[len(pg.Items)-1].ID, o)
	}
	return pg, nil
}
func (s *Store) ListPosts(ctx context.Context, id string, o f.Options) (f.Page[f.Post], error) {
	return s.posts(ctx, "posts:"+id, `p.topic_id=?`, []any{id}, o)
}
func (s *Store) Search(ctx context.Context, id, query string, o f.Options) (f.Page[f.Post], error) {
	// Treat input as text, not FTS syntax: punctuation and quotes cannot become
	// malformed expressions or unexpectedly broaden the search.
	words := strings.Fields(query)
	for i := range words {
		words[i] = `"` + strings.ReplaceAll(words[i], `"`, `""`) + `"`
	}
	q := strings.Join(words, " AND ")
	pg, err := s.posts(ctx, "search:"+id+":"+query, `p.forum_id=? AND p.id IN (SELECT post_id FROM forum_post_fts WHERE forum_post_fts MATCH ? AND forum_id=?)`, []any{id, q, id}, o)
	for i := range pg.Items {
		p := &pg.Items[i]
		body := []rune(p.BodyMD)
		lower := []rune(strings.ToLower(p.BodyMD))
		start := 0
		for _, word := range strings.Fields(query) {
			needle := []rune(strings.ToLower(word))
			found := -1
			for j := 0; j+len(needle) <= len(lower); j++ {
				if string(lower[j:j+len(needle)]) == string(needle) {
					found = j
					break
				}
			}
			if found >= 0 {
				start = max(0, found-40)
				break
			}
		}
		p.Snippet = string(body[start:min(len(body), start+160)])
	}
	return pg, err
}
func (s *Store) posts(ctx context.Context, scope, where string, args []any, o f.Options) (f.Page[f.Post], error) {
	pg := f.Page[f.Post]{Items: []f.Post{}}
	key, e := decodeCursor(scope, o)
	if e != nil {
		return pg, e
	}
	var seq int64
	if key != "" {
		if _, e = fmt.Sscan(key, &seq); e != nil || seq < 1 {
			return pg, f.ErrInvalid
		}
	}
	where += ` AND p.seq>? AND p.seq>?`
	args = append(args, seq, o.SinceSeq)
	filters, params := postFilters(o)
	where += filters
	args = append(args, params...)
	args = append(args, o.Limit+1)
	// #nosec G202 -- Query fragments are internal constants; all caller inputs are bound parameters.
	rows, e := s.db.QueryContext(ctx, `SELECT `+postCols+` FROM forum_posts p JOIN forum_topics t ON t.id=p.topic_id WHERE `+where+` ORDER BY p.seq LIMIT ?`, args...)
	if e != nil {
		return pg, e
	}
	defer rows.Close()
	for rows.Next() {
		v, e := scanPost(rows)
		if e != nil {
			return pg, e
		}
		pg.Items = append(pg.Items, v)
	}
	if e = rows.Err(); e != nil {
		return pg, e
	}
	if len(pg.Items) > o.Limit {
		pg.HasMore = true
		pg.Items = pg.Items[:o.Limit]
		pg.NextCursor = nextCursor(scope, fmt.Sprint(pg.Items[len(pg.Items)-1].Seq), o)
	}
	for _, v := range pg.Items {
		pg.LastSeq = v.Seq
	}
	return pg, nil
}

var _ f.Store = (*Store)(nil)
var _ f.Searcher = (*Store)(nil)
