package forums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	f "github.com/hollis-labs/go-forums"
	"github.com/hollis-labs/tether/internal/store"
)

func fixture(t *testing.T) (*f.Service, *store.Store, f.Caller) {
	t.Helper()
	db, e := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), db, f.Caller{Provenance: f.Provenance{Principal: f.Principal{ID: "operator", Kind: "user"}, Verified: true}, Operator: true}
}
func execute(t *testing.T, s *f.Service, c f.Caller, op string, r f.Request) map[string]any {
	t.Helper()
	v, e := s.Execute(context.Background(), c, op, r)
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	if e = json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	return m
}
func seed(t *testing.T, s *f.Service, c f.Caller) (string, string) {
	t.Helper()
	execute(t, s, c, "forum_create", f.Request{ID: "portfolio", Namespace: "app/torque/forum/portfolio", Title: "Portfolio"})
	r := execute(t, s, c, "topic_create", f.Request{ForumID: "portfolio", Title: "Architecture search title"})
	return "portfolio", r["topic"].(map[string]any)["id"].(string)
}
func TestAtomicThreadSequenceSearchAndDiscovery(t *testing.T) {
	s, db, c := fixture(t)
	forum, topic := seed(t, s, c)
	c.SessionID = "host-resolved-session"
	c.WorkstreamID = "workstream"
	c.ProjectID = "project"
	// A ref does not require FK enforcement (ADR 0008); it must be atomic with the post.
	body := strings.Repeat("🙂", 2400) + " needle-at-end"
	p := execute(t, s, c, "post_create", f.Request{TopicID: topic, BodyMD: body})["post"].(map[string]any)
	id := p["id"].(string)
	reply := execute(t, s, c, "post_create", f.Request{TopicID: topic, ParentPostID: id, BodyMD: "Reply needle"})["post"].(map[string]any)
	if reply["depth"] != float64(1) || reply["seq"] != float64(2) {
		t.Fatalf("reply: %v", reply)
	}
	outline := execute(t, s, c, "topic_get", f.Request{TopicID: topic, Options: f.Options{Limit: 1}})
	pg := outline["outline"].(map[string]any)
	items := pg["items"].([]any)
	first := items[0].(map[string]any)
	if _, ok := first["body_md"]; ok {
		t.Fatal("outline exposed full body")
	}
	if len([]rune(first["snippet"].(string))) != 160 || !first["truncated"].(bool) {
		t.Fatal("unbounded preview")
	}
	next := execute(t, s, c, "post_list", f.Request{TopicID: topic, Options: f.Options{Limit: 1, Cursor: pg["next_cursor"].(string)}})
	if next["items"].([]any)[0].(map[string]any)["id"] != reply["id"] {
		t.Fatal("pagination skipped reply")
	}
	for _, query := range []string{"Architecture", "needle"} {
		r := execute(t, s, c, "forum_search", f.Request{ForumID: forum, Query: query})
		if len(r["items"].([]any)) != 2 {
			t.Fatalf("search %q: %v", query, r)
		}
	}
	full := execute(t, s, c, "post_get", f.Request{PostID: id, Options: f.Options{Detail: "full"}})["post"].(map[string]any)
	if full["body_md"] != body || full["truncated"].(bool) {
		t.Fatal("full body did not round-trip Unicode")
	}
	refs, e := db.ListSessionRefs(c.SessionID, store.ListSessionRefsOptions{Kind: store.KindForumPost})
	if e != nil || len(refs) != 2 {
		t.Fatalf("refs: %v %v", refs, e)
	}
	recent := execute(t, s, c, "post_list", f.Request{TopicID: topic, Options: f.Options{SinceSeq: 1, WorkstreamID: "workstream", ProjectID: "project"}})
	if len(recent["items"].([]any)) != 1 {
		t.Fatal("since_seq / provenance filters")
	}
	budget := execute(t, s, c, "post_list", f.Request{TopicID: topic, Options: f.Options{MaxChars: 10}})
	sum := 0
	for _, v := range budget["items"].([]any) {
		if text, ok := v.(map[string]any)["snippet"].(string); ok {
			sum += len([]rune(text))
		}
	}
	if sum > 10 {
		t.Fatal("aggregate snippet budget exceeded")
	}
	_, e = s.Execute(context.Background(), c, "post_list", f.Request{TopicID: topic, Options: f.Options{Cursor: pg["next_cursor"].(string), Author: "other"}})
	if !errors.Is(e, f.ErrInvalid) {
		t.Fatal("cursor was reused with different filters")
	}
}
func TestAccessParentValidationAndRollback(t *testing.T) {
	s, db, c := fixture(t)
	_, topic := seed(t, s, c)
	other := f.Caller{Provenance: f.Provenance{Principal: f.Principal{ID: "other", Kind: "agent"}}}
	for _, op := range []string{"forum_get", "topic_create", "topic_list", "topic_get", "post_create", "post_list", "forum_search"} {
		_, e := s.Execute(context.Background(), other, op, f.Request{ForumID: "portfolio", TopicID: topic, Title: "No", BodyMD: "No", Query: "No"})
		if !errors.Is(e, f.ErrForbidden) {
			t.Fatalf("%s allowed outsider: %v", op, e)
		}
	}
	p := execute(t, s, c, "post_create", f.Request{TopicID: topic, BodyMD: "Root"})["post"].(map[string]any)
	second := execute(t, s, c, "topic_create", f.Request{ForumID: "portfolio", Title: "Other"})["topic"].(map[string]any)["id"].(string)
	_, e := s.Execute(context.Background(), c, "post_create", f.Request{TopicID: second, ParentPostID: p["id"].(string), BodyMD: "Wrong topic"})
	if !errors.Is(e, f.ErrInvalid) {
		t.Fatalf("cross-topic parent: %v", e)
	}
	if _, e = db.DB().Exec(`CREATE TRIGGER fail_forum_ref BEFORE INSERT ON session_refs WHEN NEW.kind='forum_post' BEGIN SELECT RAISE(ABORT,'ref unavailable'); END`); e != nil {
		t.Fatal(e)
	}
	c.SessionID = "session"
	_, e = s.Execute(context.Background(), c, "post_create", f.Request{TopicID: topic, BodyMD: "Must rollback"})
	if e == nil {
		t.Fatal("ref failure ignored")
	}
	var count, seq int
	if e = db.DB().QueryRow(`SELECT post_count,last_seq FROM forum_topics WHERE id=?`, topic).Scan(&count, &seq); e != nil {
		t.Fatal(e)
	}
	if count != 1 || seq != 1 {
		t.Fatalf("failed write advanced state: %d %d", count, seq)
	}
	r := execute(t, s, c, "forum_search", f.Request{ForumID: "portfolio", Query: "rollback"})
	if len(r["items"].([]any)) != 0 {
		t.Fatal("rolled back body remained searchable")
	}
}
func TestConcurrentPostsHaveUniqueMonotonicSequences(t *testing.T) {
	s, db, c := fixture(t)
	_, topic := seed(t, s, c)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := s.Execute(context.Background(), c, "post_create", f.Request{TopicID: topic, BodyMD: fmt.Sprint("post", i)})
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var count, unique, maximum int
	if e := db.DB().QueryRow(`SELECT COUNT(*),COUNT(DISTINCT seq),MAX(seq) FROM forum_posts`).Scan(&count, &unique, &maximum); e != nil {
		t.Fatal(e)
	}
	if count != 16 || unique != 16 || maximum != 16 {
		t.Fatalf("sequences: %d %d %d", count, unique, maximum)
	}
}

func TestQueuedPostKeepsActivityMonotonic(t *testing.T) {
	svc, db, c := fixture(t)
	_, topic := seed(t, svc, c)
	st := &Store{db: db.DB()}
	ctx := context.Background()
	newer := time.Now().UTC().Add(time.Hour)
	newestAt := newer.Format("2006-01-02T15:04:05.000000000Z")
	for i, at := range []string{newestAt, newer.Add(-time.Minute).Format("2006-01-02T15:04:05.000000000Z")} {
		_, e := st.CreatePost(ctx, f.Post{ID: fmt.Sprint("queued-", i), ForumID: "portfolio", TopicID: topic, BodyMD: "queued", BodyChars: 6, CreatedAt: at, Provenance: c.Provenance})
		if e != nil {
			t.Fatal(e)
		}
	}
	var activity, updated string
	if e := db.DB().QueryRow(`SELECT t.last_activity_at,f.updated_at FROM forum_topics t JOIN forums f ON f.id=t.forum_id WHERE t.id=?`, topic).Scan(&activity, &updated); e != nil {
		t.Fatal(e)
	}
	if activity != newestAt || updated != activity {
		t.Fatalf("activity regressed: %s %s", activity, updated)
	}
}
