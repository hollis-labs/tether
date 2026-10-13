package forums

import (
	"context"
	"errors"
	"testing"

	f "github.com/hollis-labs/go-forums"
	"github.com/hollis-labs/tether/internal/store"
)

func ptr[T any](v T) *T                            { return &v }
func objectID(m map[string]any, key string) string { return m[key].(map[string]any)["id"].(string) }
func TestHierarchyManagementLifecycle(t *testing.T) {
	s, db, c := fixture(t)
	ctx := context.Background()
	ns := "app/torque/forum/main"
	run := func(op string, r f.Request) map[string]any { t.Helper(); return execute(t, s, c, op, r) }
	fail := func(op string, r f.Request, want error) {
		t.Helper()
		_, e := s.Execute(ctx, c, op, r)
		if !errors.Is(e, want) {
			t.Fatalf("%s: got %v want %v", op, e, want)
		}
	}
	run("category_create", f.Request{ID: "planning", Namespace: ns, Title: "Planning"})
	run("category_update", f.Request{Options: f.Options{CategoryID: ptr("planning")}, Title: "Plans", Description: ptr("Plan here")})
	for _, id := range []string{"a", "b"} {
		run("forum_create", f.Request{ID: id, Namespace: ns, Title: id})
	}
	run("forum_move", f.Request{ForumID: "a", DestinationCategoryID: ptr("planning")})
	listed := run("forum_list", f.Request{Namespace: ns, Options: f.Options{CategoryID: ptr("")}})
	if rows := listed["items"].([]any); len(rows) != 1 || rows[0].(map[string]any)["id"] != "b" {
		t.Fatal(listed)
	}
	run("forum_update", f.Request{ForumID: "a", Title: "Board A", Description: ptr("Description"), Config: []byte(`{"ui":{"theme":"dark"}}`)})
	topic := objectID(run("topic_create", f.Request{ForumID: "a", Title: "Design"}), "topic")
	target := objectID(run("topic_create", f.Request{ForumID: "b", Title: "Destination"}), "topic")
	c.SessionID = "source-session"
	attachments := []f.Attachment{{Name: "diagram", URI: "https://example.org/diagram.svg", MediaType: "image/svg+xml"}}
	root := objectID(run("post_create", f.Request{TopicID: topic, BodyMD: "root content", Subject: ptr("Subjectneedle"), Attachments: &attachments}), "post")
	child := objectID(run("post_create", f.Request{TopicID: topic, ParentPostID: root, BodyMD: "child content"}), "post")
	grand := objectID(run("post_create", f.Request{TopicID: topic, ParentPostID: child, BodyMD: "grandchild"}), "post")
	if len(run("forum_search", f.Request{ForumID: "a", Query: "Subjectneedle"})["items"].([]any)) != 1 {
		t.Fatal("subject not indexed")
	}
	run("post_update", f.Request{PostID: root, BodyMD: "edited root", Subject: ptr("Updatedneedle")})
	before, _ := s.Store.GetPost(ctx, root)
	fork := objectID(run("topic_fork", f.Request{TopicID: topic, PostID: child, DestinationForumID: "b", Title: "Fork"}), "topic")
	forkPosts, e := s.Store.ListPosts(ctx, fork, f.Options{Limit: 100})
	if e != nil || len(forkPosts.Items) != 2 || forkPosts.Items[0].SourcePostID != child || forkPosts.Items[0].ParentPostID != "" || forkPosts.Items[1].ParentPostID != forkPosts.Items[0].ID {
		t.Fatalf("fork: %+v %v", forkPosts, e)
	}
	clone := objectID(run("post_clone", f.Request{PostID: root, DestinationTopicID: target}), "post")
	cloned, _ := s.Store.GetPost(ctx, clone)
	if cloned.SourcePostID != root || cloned.Provenance != before.Provenance || cloned.Subject != before.Subject || len(cloned.Attachments) != 1 {
		t.Fatal(cloned)
	}
	run("post_move", f.Request{PostID: child, DestinationTopicID: target, ParentPostID: clone})
	moved, _ := s.Store.GetPost(ctx, grand)
	if moved.ForumID != "b" || moved.TopicID != target || moved.Depth != 2 {
		t.Fatal(moved)
	}
	orig, _ := s.Store.GetTopic(ctx, topic)
	if orig.PostCount != 1 {
		t.Fatal(orig)
	}
	rootAfter, _ := s.Store.GetPost(ctx, root)
	if rootAfter.ReplyCount != 0 {
		t.Fatal(rootAfter)
	}
	refs, e := db.ListSessionRefs("source-session", store.ListSessionRefsOptions{Kind: store.KindForumPost})
	if e != nil || len(refs) != 3 {
		t.Fatalf("refs %v %v", refs, e)
	}
	for _, ref := range refs {
		if ref.RefID == grand && ref.URI != "forum://b/post/"+grand {
			t.Fatal(ref)
		}
	}
	fail("post_move", f.Request{PostID: clone, DestinationTopicID: target, ParentPostID: grand}, f.ErrInvalid)
	fail("post_delete", f.Request{PostID: clone}, f.ErrConflict)
	run("topic_move", f.Request{TopicID: topic, DestinationForumID: "b"})
	after, _ := s.Store.GetPost(ctx, root)
	if after.ID != before.ID || after.ForumID != "b" || after.Provenance != before.Provenance {
		t.Fatal(after)
	}
	if len(run("forum_search", f.Request{ForumID: "a", Query: "Updatedneedle"})["items"].([]any)) != 0 {
		t.Fatal("stale source FTS")
	}
	if len(run("forum_search", f.Request{ForumID: "b", Query: "Updatedneedle"})["items"].([]any)) != 2 {
		t.Fatal("missing destination FTS")
	}
	copied := objectID(run("topic_clone", f.Request{TopicID: topic, DestinationForumID: "a"}), "topic")
	run("topic_update", f.Request{TopicID: copied, Title: "Revised title", Options: f.Options{Status: "resolved"}})
	run("topic_archive", f.Request{TopicID: copied})
	fail("post_create", f.Request{TopicID: copied, BodyMD: "denied"}, f.ErrForbidden)
	run("topic_unarchive", f.Request{TopicID: copied})
	restored, _ := s.Store.GetTopic(ctx, copied)
	if restored.Status != "resolved" || restored.Archived {
		t.Fatal(restored)
	}
	run("topic_update", f.Request{TopicID: copied, Options: f.Options{Status: "open"}})
	run("category_archive", f.Request{Options: f.Options{CategoryID: ptr("planning")}})
	fail("topic_create", f.Request{ForumID: "a", Title: "denied"}, f.ErrForbidden)
	fail("post_create", f.Request{TopicID: copied, BodyMD: "denied"}, f.ErrForbidden)
	fail("forum_create", f.Request{ID: "blocked", Namespace: ns, Title: "denied", Options: f.Options{CategoryID: ptr("planning")}}, f.ErrForbidden)
	run("forum_archive", f.Request{ForumID: "a"})
	run("category_unarchive", f.Request{Options: f.Options{CategoryID: ptr("planning")}})
	fail("topic_create", f.Request{ForumID: "a", Title: "still denied"}, f.ErrForbidden)
	run("forum_unarchive", f.Request{ForumID: "a"})
	run("forum_clone", f.Request{ForumID: "b", ID: "copy", DestinationCategoryID: ptr("planning")})
	deep, _ := s.Store.ListTopics(ctx, "copy", f.Options{Limit: 100})
	if len(deep.Items) != 3 {
		t.Fatalf("deep clone %+v", deep)
	}
	fail("category_delete", f.Request{Options: f.Options{CategoryID: ptr("planning")}}, f.ErrConflict)
	fail("forum_delete", f.Request{ForumID: "b"}, f.ErrConflict)
	fail("topic_delete", f.Request{TopicID: topic}, f.ErrConflict)
	run("category_delete", f.Request{Options: f.Options{CategoryID: ptr("planning")}, Cascade: true})
	if _, e = s.Store.GetForum(ctx, "copy"); !errors.Is(e, f.ErrNotFound) {
		t.Fatal(e)
	}
	run("post_delete", f.Request{PostID: clone, Cascade: true})
	if _, e = s.Store.GetPost(ctx, grand); !errors.Is(e, f.ErrNotFound) {
		t.Fatal(e)
	}
	run("topic_delete", f.Request{TopicID: topic, Cascade: true})
	run("forum_delete", f.Request{ForumID: "b", Cascade: true})
	var posts, fts, refCount int
	for q, dest := range map[string]*int{`SELECT COUNT(*) FROM forum_posts`: &posts, `SELECT COUNT(*) FROM forum_post_fts`: &fts, `SELECT COUNT(*) FROM session_refs WHERE kind='forum_post'`: &refCount} {
		if e = db.DB().QueryRow(q).Scan(dest); e != nil {
			t.Fatal(e)
		}
	}
	if posts != 0 || fts != 0 || refCount != 0 {
		t.Fatalf("orphans posts=%d fts=%d refs=%d", posts, fts, refCount)
	}
}

func TestHierarchyAuthorizationAndRollback(t *testing.T) {
	s, db, c := fixture(t)
	ctx := context.Background()
	ns := "app/test/forum/main"
	owner := f.Caller{Provenance: f.Provenance{Principal: f.Principal{ID: "owner", Kind: "agent"}}}
	outsider := f.Caller{Provenance: f.Provenance{Principal: f.Principal{ID: "outsider", Kind: "agent"}}}
	execute(t, s, c, "category_create", f.Request{ID: "cat", Namespace: ns, Title: "Category"})
	for _, id := range []string{"source", "target"} {
		execute(t, s, c, "forum_create", f.Request{ID: id, Namespace: ns, Title: id, OwnerType: owner.Kind, OwnerID: owner.ID, Options: f.Options{CategoryID: ptr("cat")}})
	}
	execute(t, s, c, "forum_create", f.Request{ID: "private", Namespace: ns, Title: "Private"})
	execute(t, s, c, "forum_create", f.Request{ID: "other", Namespace: "app/other/forum/main", Title: "Other"})
	topic := objectID(execute(t, s, owner, "topic_create", f.Request{ForumID: "source", Title: "Source"}), "topic")
	post := objectID(execute(t, s, owner, "post_create", f.Request{TopicID: topic, BodyMD: "retain"}), "post")
	for _, grant := range []string{"read", "post"} {
		execute(t, s, c, "participant_add", f.Request{ForumID: "source", PrincipalKind: outsider.Kind, PrincipalID: outsider.ID, Access: grant})
		execute(t, s, outsider, "category_get", f.Request{Options: f.Options{CategoryID: ptr("cat")}})
		for _, op := range []string{"forum_update", "forum_archive", "forum_delete", "topic_update", "topic_move", "topic_clone", "post_update", "post_delete"} {
			_, e := s.Execute(ctx, outsider, op, f.Request{ForumID: "source", TopicID: topic, PostID: post, Title: "Attack", BodyMD: "Attack", DestinationForumID: "target"})
			if !errors.Is(e, f.ErrForbidden) {
				t.Fatalf("%s %s: %v", grant, op, e)
			}
		}
	}
	for _, tc := range []struct {
		caller f.Caller
		dest   string
		want   error
	}{{owner, "private", f.ErrForbidden}, {c, "other", f.ErrInvalid}} {
		_, e := s.Execute(ctx, tc.caller, "topic_move", f.Request{TopicID: topic, DestinationForumID: tc.dest})
		if !errors.Is(e, tc.want) {
			t.Fatal(e)
		}
	}
	execute(t, s, c, "participant_revoke", f.Request{ForumID: "source", PrincipalKind: outsider.Kind, PrincipalID: outsider.ID})
	if _, e := s.Execute(ctx, outsider, "category_get", f.Request{Options: f.Options{CategoryID: ptr("cat")}}); !errors.Is(e, f.ErrNotFound) {
		t.Fatal(e)
	}
	// An FTS failure halfway through a transfer must leave source, destination,
	// sequence counters and correlation data intact.
	if _, e := db.DB().Exec(`DROP TABLE forum_post_fts`); e != nil {
		t.Fatal(e)
	}
	before, _ := s.Store.GetForum(ctx, "target")
	if _, e := s.Execute(ctx, owner, "topic_move", f.Request{TopicID: topic, DestinationForumID: "target"}); e == nil {
		t.Fatal("missing FTS should fail")
	}
	after, _ := s.Store.GetForum(ctx, "target")
	p, _ := s.Store.GetPost(ctx, post)
	tp, _ := s.Store.GetTopic(ctx, topic)
	if after.LastSeq != before.LastSeq || p.ForumID != "source" || tp.ForumID != "source" {
		t.Fatal("partial move committed")
	}
}
