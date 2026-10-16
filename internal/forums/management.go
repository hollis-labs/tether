package forums

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	f "github.com/hollis-labs/libs/core/forums"
)

func stringVal(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func boolVal(v *bool) bool  { return v != nil && *v }
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

const categoryCols = `id,namespace,title,description,archived,owner_type,owner_id,created_at,updated_at,sort_order`

func scanCategory(s scanner) (f.Category, error) {
	var c f.Category
	e := s.Scan(&c.ID, &c.Namespace, &c.Title, &c.Description, &c.Archived, &c.OwnerType, &c.OwnerID, &c.CreatedAt, &c.UpdatedAt, &c.SortOrder)
	return c, mapErr(e)
}

const categoryVisible = `(? OR (owner_id=? AND owner_type=?) OR EXISTS (SELECT 1 FROM forums b WHERE b.category_id=forum_categories.id AND ((b.owner_id=? AND b.owner_type=?) OR EXISTS (SELECT 1 FROM forum_participants g WHERE g.forum_id=b.id AND g.principal_id=? AND g.principal_kind=?))))`

func (s *Store) GetCategory(ctx context.Context, c f.Caller, id string) (f.Category, error) {
	return scanCategory(s.db.QueryRowContext(ctx, `SELECT `+categoryCols+` FROM forum_categories WHERE id=? AND `+categoryVisible, id, c.Operator, c.ID, c.Kind, c.ID, c.Kind, c.ID, c.Kind))
}
func (s *Store) ListCategories(ctx context.Context, c f.Caller, o f.Options) (f.Page[f.Category], error) {
	pg := f.Page[f.Category]{Items: []f.Category{}}
	scope := "categories:" + c.Kind + ":" + c.ID
	key, e := decodeOrderCursor(scope, o)
	if e != nil {
		return pg, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT `+categoryCols+` FROM forum_categories WHERE (? OR (sort_order,id)>(?,?)) AND (?='' OR namespace=?) AND (? OR archived=?) AND `+categoryVisible+` ORDER BY sort_order,id LIMIT ?`, o.Cursor == "", key.SortOrder, key.ID, o.NamespaceFilter, o.NamespaceFilter, o.Archived == nil, boolVal(o.Archived), c.Operator, c.ID, c.Kind, c.ID, c.Kind, c.ID, c.Kind, o.Limit+1)
	if e != nil {
		return pg, e
	}
	defer rows.Close()
	for rows.Next() {
		c, e := scanCategory(rows)
		if e != nil {
			return pg, e
		}
		pg.Items = append(pg.Items, c)
	}
	if e = rows.Err(); e != nil {
		return pg, e
	}
	if len(pg.Items) > o.Limit {
		pg.HasMore = true
		pg.Items = pg.Items[:o.Limit]
		pg.NextCursor = nextCursor(scope, orderKey(pg.Items[len(pg.Items)-1].SortOrder, pg.Items[len(pg.Items)-1].ID), o)
	}
	return pg, nil
}
func getForum(ctx context.Context, tx *sql.Tx, id string) (f.Forum, error) {
	return scanForum(tx.QueryRowContext(ctx, `SELECT `+forumCols+` FROM forums WHERE id=?`, id))
}
func getTopic(ctx context.Context, tx *sql.Tx, id string) (f.Topic, error) {
	return scanTopic(tx.QueryRowContext(ctx, `SELECT `+topicCols+` FROM forum_topics WHERE id=?`, id))
}
func getPost(ctx context.Context, tx *sql.Tx, id string) (f.Post, error) {
	return scanPost(tx.QueryRowContext(ctx, `SELECT `+postCols+` FROM forum_posts p JOIN forum_topics t ON t.id=p.topic_id WHERE p.id=?`, id))
}
func writableForum(ctx context.Context, tx *sql.Tx, id string) error {
	var archived bool
	e := tx.QueryRowContext(ctx, `SELECT (b.archived OR COALESCE(c.archived,0)) FROM forums b LEFT JOIN forum_categories c ON c.id=b.category_id WHERE b.id=?`, id).Scan(&archived)
	if e != nil {
		return mapErr(e)
	}
	if archived {
		return fmt.Errorf("%w: forum or category is archived", f.ErrForbidden)
	}
	return nil
}
func writableTopic(ctx context.Context, tx *sql.Tx, id string) error {
	t, e := getTopic(ctx, tx, id)
	if e != nil {
		return e
	}
	if t.Archived || t.Status != "open" {
		return fmt.Errorf("%w: topic is not open", f.ErrForbidden)
	}
	return writableForum(ctx, tx, t.ForumID)
}
func categoryTarget(ctx context.Context, tx *sql.Tx, id, ns string) error {
	if id == "" {
		return nil
	}
	var target string
	var archived bool
	e := tx.QueryRowContext(ctx, `SELECT namespace,archived FROM forum_categories WHERE id=?`, id).Scan(&target, &archived)
	if e != nil {
		return mapErr(e)
	}
	if target != ns {
		return fmt.Errorf("%w: category namespace mismatch", f.ErrInvalid)
	}
	if archived {
		return f.ErrForbidden
	}
	return nil
}
func nextSeq(ctx context.Context, tx *sql.Tx, id, at string) (int64, error) {
	var seq int64
	e := tx.QueryRowContext(ctx, `UPDATE forums SET last_seq=last_seq+1,updated_at=MAX(updated_at,?) WHERE id=? RETURNING last_seq`, at, id).Scan(&seq)
	return seq, mapErr(e)
}
func insertForum(ctx context.Context, tx *sql.Tx, v f.Forum) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO forums (`+forumCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.Namespace, v.Title, v.OwnerType, v.OwnerID, string(v.Config), jsonText(v.Policy), string(v.Metadata), v.CreatedAt, v.UpdatedAt, v.LastSeq, v.CategoryID, v.Description, v.Archived, v.SortOrder)
	return mapErr(e)
}
func insertTopic(ctx context.Context, tx *sql.Tx, v f.Topic) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO forum_topics (`+topicCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.ForumID, v.Title, v.Status, v.CreatedBy.ID, v.CreatedBy.Kind, v.PostCount, v.CreatedAt, v.LastActivityAt, v.LastSeq, v.SourceTopicID, v.Archived)
	return mapErr(e)
}
func insertPost(ctx context.Context, tx *sql.Tx, v f.Post) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO forum_posts (id,topic_id,forum_id,parent_post_id,seq,body_md,body_chars,author_id,author_kind,verified,session_id,agent_urn,workstream_id,project_id,launch_id,created_at,edited_at,depth,reply_count,subject,attachments_json,source_post_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.TopicID, v.ForumID, v.ParentPostID, v.Seq, v.BodyMD, v.BodyChars, v.Provenance.ID, v.Provenance.Kind, v.Provenance.Verified, v.Provenance.SessionID, v.Provenance.AgentURN, v.Provenance.WorkstreamID, v.Provenance.ProjectID, v.Provenance.LaunchID, v.CreatedAt, v.EditedAt, v.Depth, v.ReplyCount, v.Subject, jsonText(v.Attachments), v.SourcePostID)
	return mapErr(e)
}
func topicPosts(ctx context.Context, tx *sql.Tx, id, root string) ([]f.Post, error) {
	q := `SELECT ` + postCols + ` FROM forum_posts p JOIN forum_topics t ON t.id=p.topic_id WHERE p.topic_id=?`
	args := []any{id}
	if root != "" {
		q += ` AND p.id IN (WITH RECURSIVE subtree(id) AS (SELECT id FROM forum_posts WHERE id=? AND topic_id=? UNION ALL SELECT child.id FROM forum_posts child JOIN subtree ON child.parent_post_id=subtree.id) SELECT id FROM subtree)`
		args = append(args, root, id)
	}
	q += ` ORDER BY p.depth,p.seq`
	rows, e := tx.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []f.Post{}
	for rows.Next() {
		p, e := scanPost(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func refreshTopic(ctx context.Context, tx *sql.Tx, id, at string) error {
	// Recompute affected derived state in the mutation transaction, including edits,
	// deletes and moves whose original timestamps predate the current high-water mark.
	_, e := tx.ExecContext(ctx, `UPDATE forum_posts SET reply_count=(SELECT COUNT(*) FROM forum_posts child WHERE child.parent_post_id=forum_posts.id) WHERE topic_id=?`, id)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `UPDATE forum_topics SET post_count=(SELECT COUNT(*) FROM forum_posts WHERE topic_id=?),last_activity_at=MAX(last_activity_at,?),last_seq=(SELECT last_seq FROM forums WHERE id=forum_topics.forum_id) WHERE id=?`, id, at, id)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `DELETE FROM forum_post_fts WHERE post_id IN (SELECT id FROM forum_posts WHERE topic_id=?)`, id)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO forum_post_fts (forum_id,post_id,topic_title,body_md) SELECT p.forum_id,p.id,t.title,p.subject || ' ' || p.body_md FROM forum_posts p JOIN forum_topics t ON t.id=p.topic_id WHERE p.topic_id=?`, id)
	return e
}
func deletePosts(ctx context.Context, tx *sql.Tx, posts []f.Post) error {
	for _, p := range posts {
		for _, q := range []string{`DELETE FROM forum_post_fts WHERE post_id=?`, `DELETE FROM session_refs WHERE kind='forum_post' AND ref_id=?`, `DELETE FROM forum_posts WHERE id=?`} {
			if _, e := tx.ExecContext(ctx, q, p.ID); e != nil {
				return e
			}
		}
	}
	return nil
}
func deleteTopic(ctx context.Context, tx *sql.Tx, id string, cascade bool) error {
	posts, e := topicPosts(ctx, tx, id, "")
	if e != nil {
		return e
	}
	if len(posts) > 0 && !cascade {
		return fmt.Errorf("%w: topic is not empty; use cascade", f.ErrConflict)
	}
	if e = deletePosts(ctx, tx, posts); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `DELETE FROM forum_topics WHERE id=?`, id)
	return e
}
func forumTopics(ctx context.Context, tx *sql.Tx, id string) ([]f.Topic, error) {
	rows, e := tx.QueryContext(ctx, `SELECT `+topicCols+` FROM forum_topics WHERE forum_id=? ORDER BY id`, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []f.Topic{}
	for rows.Next() {
		t, e := scanTopic(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func deleteForum(ctx context.Context, tx *sql.Tx, id string, cascade bool) error {
	ts, e := forumTopics(ctx, tx, id)
	if e != nil {
		return e
	}
	if len(ts) > 0 && !cascade {
		return fmt.Errorf("%w: forum is not empty; use cascade", f.ErrConflict)
	}
	for _, t := range ts {
		if e = deleteTopic(ctx, tx, t.ID, true); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM forum_participants WHERE forum_id=?`, id); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `DELETE FROM forums WHERE id=?`, id)
	return e
}

// transferPosts preserves provenance and reply structure. A post transfer always
// includes descendants. A supplied parent must be outside the moving subtree.
func transferPosts(ctx context.Context, tx *sql.Tx, posts []f.Post, dest f.Topic, parent string, clone bool, at string, newID func() string) (string, error) {
	if len(posts) == 0 {
		return "", nil
	}
	ids := map[string]string{}
	minDepth := posts[0].Depth
	for _, p := range posts {
		ids[p.ID] = p.ID
		if clone {
			ids[p.ID] = newID()
		}
		minDepth = min(minDepth, p.Depth)
	}
	baseDepth := 0
	if parent != "" {
		p, e := getPost(ctx, tx, parent)
		if e != nil {
			return "", e
		}
		if p.TopicID != dest.ID {
			return "", fmt.Errorf("%w: parent must be in destination topic", f.ErrInvalid)
		}
		if _, inside := ids[parent]; inside && !clone {
			return "", fmt.Errorf("%w: reply cycle", f.ErrInvalid)
		}
		baseDepth = p.Depth + 1
	}
	for _, p := range posts {
		oldID := p.ID
		oldParent := p.ParentPostID
		p.ID = ids[oldID]
		p.TopicID = dest.ID
		p.ForumID = dest.ForumID
		p.Depth = p.Depth - minDepth + baseDepth
		if p.Depth > 1000 {
			return "", fmt.Errorf("%w: thread depth exceeds 1000", f.ErrInvalid)
		}
		p.ParentPostID = parent
		if mapped, ok := ids[oldParent]; ok {
			p.ParentPostID = mapped
		}
		var e error
		p.Seq, e = nextSeq(ctx, tx, dest.ForumID, at)
		if e != nil {
			return "", e
		}
		if clone {
			p.SourcePostID = oldID
			e = insertPost(ctx, tx, p)
		} else {
			_, e = tx.ExecContext(ctx, `UPDATE forum_posts SET topic_id=?,forum_id=?,parent_post_id=?,seq=?,depth=? WHERE id=?`, p.TopicID, p.ForumID, p.ParentPostID, p.Seq, p.Depth, p.ID)
			if e == nil {
				_, e = tx.ExecContext(ctx, `UPDATE session_refs SET uri=? WHERE kind='forum_post' AND ref_id=?`, "forum://"+p.ForumID+"/post/"+p.ID, p.ID)
			}
		}
		if e != nil {
			return "", e
		}
		// A clone is a copy, not a new assertion that the original session created it.
		// Keep source_post_id and original provenance; original correlation refs stay
		// on the source. Moves update existing refs atomically.
	}
	return ids[posts[0].ID], refreshTopic(ctx, tx, dest.ID, at)
}
func cloneTopic(ctx context.Context, tx *sql.Tx, t f.Topic, dest, title, root, at string, newID func() string) (f.Topic, error) {
	posts, e := topicPosts(ctx, tx, t.ID, root)
	if e != nil {
		return t, e
	}
	if root != "" && len(posts) == 0 {
		return t, f.ErrInvalid
	}
	t.SourceTopicID = t.ID
	t.ID = newID()
	t.ForumID = dest
	t.PostCount = 0
	t.LastSeq = 0
	t.Archived = false
	if t.Status == "archived" {
		t.Status = "open"
	}
	if title != "" {
		t.Title = title
	}
	t.CreatedAt = at
	t.LastActivityAt = at
	if e = insertTopic(ctx, tx, t); e != nil {
		return t, e
	}
	if _, e = transferPosts(ctx, tx, posts, t, "", true, at, newID); e != nil {
		return t, e
	}
	return getTopic(ctx, tx, t.ID)
}

func (s *Store) Mutate(ctx context.Context, op string, r f.Request, c f.Caller, at string, newID func() string) (any, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	var out any
	if strings.HasPrefix(op, "category_") {
		out, e = mutateCategory(ctx, tx, op, r, c, at)
	} else {
		out, e = mutateContent(ctx, tx, op, r, c, at, newID)
	}
	if e != nil {
		return nil, mapErr(e)
	}
	if e = tx.Commit(); e != nil {
		return nil, mapErr(e)
	}
	return out, nil
}
func mutateCategory(ctx context.Context, tx *sql.Tx, op string, r f.Request, c f.Caller, at string) (any, error) {
	if !c.Operator {
		return nil, f.ErrForbidden
	}
	var v f.Category
	var e error
	if op == "category_create" {
		v = f.Category{SortOrder: sortVal(r.SortOrder), ID: r.ID, Namespace: r.Namespace, Title: r.Title, Description: stringVal(r.Description), OwnerType: c.Kind, OwnerID: c.ID, CreatedAt: at, UpdatedAt: at}
		_, e = tx.ExecContext(ctx, `INSERT INTO forum_categories (`+categoryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`, v.ID, v.Namespace, v.Title, v.Description, v.Archived, v.OwnerType, v.OwnerID, v.CreatedAt, v.UpdatedAt, v.SortOrder)
	} else {
		v, e = scanCategory(tx.QueryRowContext(ctx, `SELECT `+categoryCols+` FROM forum_categories WHERE id=?`, stringVal(r.CategoryID)))
		if e != nil {
			return nil, e
		}
		switch op {
		case "category_delete":
			rows, err := tx.QueryContext(ctx, `SELECT id FROM forums WHERE category_id=?`, v.ID)
			if err != nil {
				return nil, err
			}
			ids := []string{}
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					_ = rows.Close()
					return nil, err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
			if len(ids) > 0 && !r.Cascade {
				return nil, fmt.Errorf("%w: category is not empty; use cascade", f.ErrConflict)
			}
			for _, id := range ids {
				if e = deleteForum(ctx, tx, id, true); e != nil {
					return nil, e
				}
			}
			_, e = tx.ExecContext(ctx, `DELETE FROM forum_categories WHERE id=?`, v.ID)
			return map[string]any{"deleted": v.ID}, e
		case "category_archive":
			v.Archived = true
		case "category_unarchive":
			v.Archived = false
		case "category_update":
			if r.SortOrder != nil {
				v.SortOrder = *r.SortOrder
			}
			if v.Archived {
				return nil, f.ErrForbidden
			}
			if r.Title != "" {
				v.Title = r.Title
			}
			if r.Description != nil {
				v.Description = *r.Description
			}
		default:
			return nil, f.ErrInvalid
		}
		v.UpdatedAt = at
		_, e = tx.ExecContext(ctx, `UPDATE forum_categories SET title=?,description=?,archived=?,updated_at=?,sort_order=? WHERE id=?`, v.Title, v.Description, v.Archived, at, v.SortOrder, v.ID)
	}
	return map[string]any{"category": v, "next": "tether_forum_category_get"}, e
}

func mutateContent(ctx context.Context, tx *sql.Tx, op string, r f.Request, c f.Caller, at string, newID func() string) (any, error) {
	var b f.Forum
	var t f.Topic
	var p f.Post
	var e error
	switch {
	case strings.HasPrefix(op, "forum_"):
		b, e = getForum(ctx, tx, r.ForumID)
	case strings.HasPrefix(op, "topic_"):
		t, e = getTopic(ctx, tx, r.TopicID)
		if e == nil {
			b, e = getForum(ctx, tx, t.ForumID)
		}
	case strings.HasPrefix(op, "post_"):
		p, e = getPost(ctx, tx, r.PostID)
		if e == nil {
			t, e = getTopic(ctx, tx, p.TopicID)
		}
		if e == nil {
			b, e = getForum(ctx, tx, t.ForumID)
		}
	default:
		return nil, f.ErrInvalid
	}
	if e != nil {
		return nil, e
	}
	if !f.CanAccess(b, c) {
		return nil, f.ErrForbidden
	}
	// Archived content may be read, copied, restored or explicitly deleted.
	if !strings.HasSuffix(op, "_clone") && !strings.HasSuffix(op, "_fork") && !strings.HasSuffix(op, "_delete") && !strings.HasSuffix(op, "_unarchive") && !strings.HasSuffix(op, "_archive") {
		if e = writableForum(ctx, tx, b.ID); e != nil {
			return nil, e
		}
		if t.Archived {
			return nil, f.ErrForbidden
		}
	}
	dest := b
	if r.DestinationForumID != "" {
		dest, e = getForum(ctx, tx, r.DestinationForumID)
		if e != nil {
			return nil, e
		}
		if !f.CanAccess(dest, c) {
			return nil, f.ErrForbidden
		}
		if dest.Namespace != b.Namespace {
			return nil, f.ErrInvalid
		}
		if e = writableForum(ctx, tx, dest.ID); e != nil {
			return nil, e
		}
	}
	switch op {
	case "forum_delete":
		e = deleteForum(ctx, tx, b.ID, r.Cascade)
		return map[string]any{"deleted": b.ID}, e
	case "forum_archive", "forum_unarchive", "forum_update", "forum_move":
		switch op {
		case "forum_archive":
			b.Archived = true
		case "forum_unarchive":
			b.Archived = false
		case "forum_update":
			if r.Title != "" {
				b.Title = r.Title
			}
			if r.Description != nil {
				b.Description = *r.Description
			}
			if r.Config != nil {
				b.Config = r.Config
			}
			if r.Metadata != nil {
				b.Metadata = r.Metadata
			}
		case "forum_move":
			if e = categoryTarget(ctx, tx, stringVal(r.DestinationCategoryID), b.Namespace); e != nil {
				return nil, e
			}
			b.CategoryID = stringVal(r.DestinationCategoryID)
		}
		b.UpdatedAt = at
		if r.SortOrder != nil && (op == "forum_update" || op == "forum_move") {
			b.SortOrder = *r.SortOrder
		}
		_, e = tx.ExecContext(ctx, `UPDATE forums SET title=?,description=?,config_json=?,metadata_json=?,archived=?,category_id=?,updated_at=?,sort_order=? WHERE id=?`, b.Title, b.Description, string(b.Config), string(b.Metadata), b.Archived, b.CategoryID, at, b.SortOrder, b.ID)
		return map[string]any{"forum": b}, e
	case "forum_clone":
		if r.SortOrder != nil {
			b.SortOrder = *r.SortOrder
		}
		source := b.ID
		b.ID = r.ID
		b.OwnerID = c.ID
		b.OwnerType = c.Kind
		b.Archived = false
		b.LastSeq = 0
		b.CreatedAt = at
		b.UpdatedAt = at
		if r.Title != "" {
			b.Title = r.Title
		}
		if r.DestinationCategoryID != nil {
			b.CategoryID = *r.DestinationCategoryID
		}
		if e = categoryTarget(ctx, tx, b.CategoryID, b.Namespace); e != nil {
			return nil, e
		}
		if e = insertForum(ctx, tx, b); e != nil {
			return nil, e
		}
		topics, e := forumTopics(ctx, tx, source)
		if e != nil {
			return nil, e
		}
		for _, topic := range topics {
			if _, e = cloneTopic(ctx, tx, topic, b.ID, "", "", at, newID); e != nil {
				return nil, e
			}
		}
		b, e = getForum(ctx, tx, b.ID)
		return map[string]any{"forum": b}, e
	case "topic_delete":
		if _, e = nextSeq(ctx, tx, b.ID, at); e != nil {
			return nil, e
		}
		e = deleteTopic(ctx, tx, t.ID, r.Cascade)
		return map[string]any{"deleted": t.ID}, e
	case "topic_update", "topic_archive", "topic_unarchive":
		if op == "topic_update" && r.Title != "" {
			t.Title = r.Title
		}
		switch op {
		case "topic_update":
			if r.Status != "" {
				if r.Status == "archived" {
					t.Archived = true
				} else {
					t.Status = r.Status
				}
			}
		case "topic_archive":
			t.Archived = true
		case "topic_unarchive":
			t.Archived = false
			if t.Status == "archived" {
				t.Status = "open"
			}
		}
		if _, e = nextSeq(ctx, tx, b.ID, at); e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, `UPDATE forum_topics SET title=?,status=?,archived=? WHERE id=?`, t.Title, t.Status, t.Archived, t.ID)
		if e != nil {
			return nil, e
		}
		if e = refreshTopic(ctx, tx, t.ID, at); e != nil {
			return nil, e
		}
		t, e = getTopic(ctx, tx, t.ID)
		return map[string]any{"topic": t}, e
	case "topic_clone", "topic_fork":
		root := ""
		if op == "topic_fork" {
			root = r.PostID
		}
		v, e := cloneTopic(ctx, tx, t, dest.ID, r.Title, root, at, newID)
		return map[string]any{"topic": v}, e
	case "topic_move":
		if dest.ID == b.ID {
			return map[string]any{"topic": t}, nil
		}
		posts, e := topicPosts(ctx, tx, t.ID, "")
		if e != nil {
			return nil, e
		}
		if _, e = nextSeq(ctx, tx, b.ID, at); e != nil {
			return nil, e
		}
		t.ForumID = dest.ID
		_, e = tx.ExecContext(ctx, `UPDATE forum_topics SET forum_id=? WHERE id=?`, dest.ID, t.ID)
		if e != nil {
			return nil, e
		}
		if _, e = nextSeq(ctx, tx, dest.ID, at); e != nil {
			return nil, e
		}
		if _, e = transferPosts(ctx, tx, posts, t, "", false, at, newID); e != nil {
			return nil, e
		}
		if e = refreshTopic(ctx, tx, t.ID, at); e != nil {
			return nil, e
		}
		t, e = getTopic(ctx, tx, t.ID)
		return map[string]any{"topic": t}, e
	case "post_update":
		if t.Status != "open" {
			return nil, f.ErrForbidden
		}
		if r.BodyMD != "" {
			p.BodyMD = r.BodyMD
			p.BodyChars = len([]rune(r.BodyMD))
		}
		if r.Subject != nil {
			p.Subject = *r.Subject
		}
		if r.Attachments != nil {
			p.Attachments = *r.Attachments
		}
		p.Seq, e = nextSeq(ctx, tx, b.ID, at)
		if e != nil {
			return nil, e
		}
		p.EditedAt = at
		_, e = tx.ExecContext(ctx, `UPDATE forum_posts SET body_md=?,body_chars=?,subject=?,attachments_json=?,edited_at=?,seq=? WHERE id=?`, p.BodyMD, p.BodyChars, p.Subject, jsonText(p.Attachments), p.EditedAt, p.Seq, p.ID)
		if e != nil {
			return nil, e
		}
		if e = refreshTopic(ctx, tx, t.ID, at); e != nil {
			return nil, e
		}
		return map[string]any{"post": p}, nil
	case "post_delete":
		posts, e := topicPosts(ctx, tx, t.ID, p.ID)
		if e != nil {
			return nil, e
		}
		if len(posts) > 1 && !r.Cascade {
			return nil, fmt.Errorf("%w: post has replies; use cascade", f.ErrConflict)
		}
		if _, e = nextSeq(ctx, tx, b.ID, at); e != nil {
			return nil, e
		}
		if e = deletePosts(ctx, tx, posts); e != nil {
			return nil, e
		}
		e = refreshTopic(ctx, tx, t.ID, at)
		return map[string]any{"deleted": p.ID}, e
	case "post_move", "post_clone":
		target, e := getTopic(ctx, tx, r.DestinationTopicID)
		if e != nil {
			return nil, e
		}
		// Re-resolve under this transaction; don't trust a preflight forum lookup.
		targetBoard, e := getForum(ctx, tx, target.ForumID)
		if e != nil {
			return nil, e
		}
		if !f.CanAccess(targetBoard, c) {
			return nil, f.ErrForbidden
		}
		if targetBoard.Namespace != b.Namespace {
			return nil, f.ErrInvalid
		}
		if e = writableTopic(ctx, tx, target.ID); e != nil {
			return nil, e
		}
		posts, e := topicPosts(ctx, tx, t.ID, p.ID)
		if e != nil {
			return nil, e
		}
		if op == "post_move" {
			if _, e = nextSeq(ctx, tx, b.ID, at); e != nil {
				return nil, e
			}
		}
		id, e := transferPosts(ctx, tx, posts, target, r.ParentPostID, op == "post_clone", at, newID)
		if e != nil {
			return nil, e
		}
		if op == "post_move" {
			if e = refreshTopic(ctx, tx, t.ID, at); e != nil {
				return nil, e
			}
		}
		v, e := getPost(ctx, tx, id)
		return map[string]any{"post": v}, e
	}
	return nil, f.ErrInvalid
}

var _ f.ManagementStore = (*Store)(nil)

func sortVal(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Sorting and cursor boundaries use the same tuple, including an ID tie-breaker.
type orderCursor struct {
	SortOrder int64  `json:"order"`
	ID        string `json:"id"`
}

func orderKey(order int64, id string) string { return jsonText(orderCursor{order, id}) }
func decodeOrderCursor(scope string, o f.Options) (orderCursor, error) {
	var c orderCursor
	key, err := decodeCursor(scope, o)
	if err != nil || key == "" {
		return c, err
	}
	if json.Unmarshal([]byte(key), &c) != nil || c.ID == "" {
		return c, fmt.Errorf("%w: invalid sort cursor", f.ErrInvalid)
	}
	return c, nil
}
