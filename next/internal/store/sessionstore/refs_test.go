package sessionstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// refsRace runs fn on n goroutines at once and returns their results.
func refsRace[T any](n int, fn func() (T, error)) ([]T, []error) {
	out, errs := make([]T, n), make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i], errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return out, errs
}

func TestRefsAttachAndList(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		ws := row("ses-a")
		ws.WorkstreamID = "ws-1"
		if _, err := ss.CreateSession(ctx, ws, "", nil); err != nil {
			t.Fatal(err)
		}
		ref := sessionstore.SessionRefRow{SessionID: "ses-a", Kind: sessionstore.KindTesseractRevision, RefID: "rev-1", URI: "tesseract://rev-1"}
		if res, err := ss.AttachSessionRef(ctx, ref); err != nil || !res.Inserted {
			t.Fatalf("first attach %+v %v", res, err)
		}
		clock.Advance(time.Minute)
		// A repeat from an agent changes nothing; one with a parent fills the unset parent only.
		if res, err := ss.AttachSessionRef(ctx, ref); err != nil || res.Inserted || res.Upgraded {
			t.Fatalf("repeat %+v %v", res, err)
		}
		withParent := ref
		withParent.ParentItemID, withParent.URI = "item-1", "changed"
		if res, _ := ss.AttachSessionRef(ctx, withParent); res.Inserted || res.Upgraded {
			t.Fatalf("parent fill %+v", res)
		}
		proxy := ref
		proxy.Source, proxy.ParentItemID = sessionstore.SourceProxy, "item-2"
		if res, _ := ss.AttachSessionRef(ctx, proxy); !res.Upgraded {
			t.Fatalf("proxy upgrade %+v", res)
		}
		if res, _ := ss.AttachSessionRef(ctx, proxy); res.Upgraded || res.Inserted {
			t.Fatalf("proxy is terminal %+v", res)
		}
		claimed := ref
		claimed.Source = sessionstore.SourceProxyClaimed
		if res, _ := ss.AttachSessionRef(ctx, claimed); res.Upgraded {
			t.Fatal("proxy_claimed upgraded a row")
		}
		got, err := ss.ListSessionRefs(ctx, "ses-a", sessionstore.ListSessionRefsOptions{})
		if err != nil || len(got) != 1 {
			t.Fatalf("list %+v %v", got, err)
		}
		r := got[0]
		if r.Source != sessionstore.SourceProxy || r.ParentItemID != "item-1" || r.URI != "tesseract://rev-1" || r.At != t0.Format(time.RFC3339) {
			t.Fatalf("stored ref %+v", r)
		}
		if k, id := r.CanonicalRef(); k != sessionstore.KindTesseractItem || id != "item-1" {
			t.Fatalf("canonical %s %s", k, id)
		}
		if _, err := ss.AttachSessionRef(ctx, sessionstore.SessionRefRow{SessionID: "ses-a", Kind: "k", RefID: "x", Relation: "bogus"}); err == nil {
			t.Fatal("invalid relation accepted")
		}
		if _, err := ss.AttachSessionRef(ctx, sessionstore.SessionRefRow{SessionID: "ses-a", Kind: "k", RefID: "x", Source: "bogus"}); err == nil {
			t.Fatal("invalid source accepted")
		}
		if _, err := ss.AttachSessionRef(ctx, sessionstore.SessionRefRow{SessionID: "ses-a", Kind: sessionstore.KindGitCommit, RefID: "abc", Relation: sessionstore.RelationCreated, At: "2026-11-14T12:05:00Z"}); err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			opts sessionstore.ListSessionRefsOptions
			want int
		}{
			"kind":     {sessionstore.ListSessionRefsOptions{Kind: sessionstore.KindGitCommit}, 1},
			"relation": {sessionstore.ListSessionRefsOptions{Relation: sessionstore.RelationReferenced}, 1},
			"source":   {sessionstore.ListSessionRefsOptions{Source: sessionstore.SourceProxy}, 1},
			"since":    {sessionstore.ListSessionRefsOptions{Since: "2026-11-14T12:01:00Z"}, 1},
			"limit":    {sessionstore.ListSessionRefsOptions{Limit: 1}, 1},
			"all":      {sessionstore.ListSessionRefsOptions{}, 2},
		} {
			got, err := ss.ListSessionRefs(ctx, "ses-a", c.opts)
			if err != nil || len(got) != c.want {
				t.Errorf("%s: %d %v, want %d", name, len(got), err, c.want)
			}
		}
		all, _ := ss.ListWorkstreamRefs(ctx, "ws-1", sessionstore.ListSessionRefsOptions{})
		if len(all) != 2 || all[0].Kind != sessionstore.KindGitCommit {
			t.Fatalf("workstream refs %+v", all)
		}
		if err := ss.SetSessionRefAttribution(ctx, "ses-a", sessionstore.RefAttributionProxy); err != nil {
			t.Fatal(err)
		}
		if got, _ := ss.GetSession(ctx, "ses-a"); got.RefAttribution != "proxy" {
			t.Fatalf("attribution %q", got.RefAttribution)
		}
		if err := ss.SetSessionRefAttribution(ctx, "nope", "none"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("unknown session: %v", err)
		}
		if err := ss.SetSessionRefAttribution(ctx, "ses-a", ""); err == nil {
			t.Fatal("empty attribution accepted")
		}
	})
}

// TestRefsAttachConcurrent races attaches of one ref (they overlap on Postgres): exactly one
// inserts, and then of concurrent proxy observations exactly one upgrades; none fails.
func TestRefsAttachConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		// Warm the pool so the racers do not stagger on opening connections.
		refsRace(6, func() (struct{}, error) {
			return struct{}{}, s.C().WriteTx(ctx, func(context.Context, store.Tx) error { time.Sleep(50 * time.Millisecond); return nil })
		})
		ref := sessionstore.SessionRefRow{SessionID: "ses-r", Kind: sessionstore.KindTorqueTask, RefID: "T-1"}
		count := func(results []sessionstore.AttachRefResult, errs []error) (inserted, upgraded int) {
			t.Helper()
			for i, r := range results {
				if errs[i] != nil {
					t.Fatalf("concurrent attach: %v", errs[i])
				}
				if r.Inserted {
					inserted++
				}
				if r.Upgraded {
					upgraded++
				}
			}
			return inserted, upgraded
		}
		if ins, up := count(refsRace(6, func() (sessionstore.AttachRefResult, error) { return ss.AttachSessionRef(ctx, ref) })); ins != 1 || up != 0 {
			t.Fatalf("agent attaches: inserted %d upgraded %d", ins, up)
		}
		proxy := ref
		proxy.Source = sessionstore.SourceProxy
		if ins, up := count(refsRace(6, func() (sessionstore.AttachRefResult, error) { return ss.AttachSessionRef(ctx, proxy) })); ins != 0 || up != 1 {
			t.Fatalf("proxy attaches: inserted %d upgraded %d", ins, up)
		}
	})
}

func TestRefsGroupsLineageRoute(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		for _, g := range []sessionstore.SessionGroupRow{
			{ID: "g-1", Name: "first", CreatedAt: "2026-11-14T00:00:00Z", UpdatedAt: "2026-11-14T00:00:00Z"},
			{ID: "g-2", WorkflowID: "wf", CreatedAt: "2026-11-14T00:00:00Z", UpdatedAt: "2026-11-14T00:00:00Z"},
		} {
			if err := ss.CreateSessionGroup(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
		if g, err := ss.GetSessionGroup(ctx, "g-1"); err != nil || g.Name != "first" || g.WorkflowID != "" {
			t.Fatalf("group %+v %v", g, err)
		}
		if _, err := ss.GetSessionGroup(ctx, "nope"); !errors.Is(err, sessionstore.ErrSessionGroupNotFound) {
			t.Fatalf("missing group: %v", err)
		}
		if gs, _ := ss.ListSessionGroups(ctx); len(gs) != 2 || gs[0].ID != "g-2" {
			t.Fatalf("groups %+v", gs)
		}

		root := row("ses-root")
		root.RouteJSON = `{"channel":"news"}`
		mk := func(r sessionstore.SessionRow) {
			t.Helper()
			if _, err := ss.CreateSession(ctx, r, "", nil); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Second)
		}
		mk(root)
		child := row("ses-child")
		child.ParentSessionID = "ses-root"
		mk(child)
		ended := row("ses-ended")
		ended.ParentSessionID, ended.State = "ses-root", "exited"
		mk(ended)
		grand := row("ses-grand")
		grand.ParentSessionID = "ses-child"
		mk(grand)

		if err := ss.AddSessionToGroup(ctx, "ses-child", "g-1"); err != nil {
			t.Fatal(err)
		}
		if err := ss.AddSessionToGroup(ctx, "nope", "g-1"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("missing session: %v", err)
		}
		if in, _ := ss.ListSessionsByGroup(ctx, "g-1"); len(in) != 1 || in[0].ID != "ses-child" {
			t.Fatalf("group sessions %+v", in)
		}
		if ids, _ := ss.ChildSessionIDs(ctx, "ses-root"); len(ids) != 2 || ids[0] != "ses-child" || ids[1] != "ses-ended" {
			t.Fatalf("children %v", ids)
		}
		for id, want := range map[string]int{"ses-root": 0, "ses-child": 1, "ses-grand": 2} {
			if d, err := ss.SessionLineageDepth(ctx, id); err != nil || d != want {
				t.Errorf("depth %s = %d %v, want %d", id, d, err, want)
			}
		}
		if _, err := ss.SessionLineageDepth(ctx, "nope"); err == nil {
			t.Fatal("depth of a missing session")
		}
		if n, _ := ss.LiveChildCount(ctx, "ses-root"); n != 1 {
			t.Fatalf("live children %d", n)
		}
		if n, _ := ss.ChildCountSince(ctx, "ses-root", t0.Add(2*time.Second).Format(time.RFC3339)); n != 1 {
			t.Fatalf("children since %d", n)
		}
		if id, _ := ss.LatestSessionForLogicalAgent(ctx, "agent"); id != "ses-grand" {
			t.Fatalf("latest %q", id)
		}
		if id, err := ss.LatestSessionForLogicalAgent(ctx, "nobody"); err != nil || id != "" {
			t.Fatalf("latest of nobody %q %v", id, err)
		}
		if route, err := ss.SessionRouteJSON(ctx, "ses-root"); err != nil || route != `{"channel":"news"}` {
			t.Fatalf("route %q %v", route, err)
		}
		if route, err := ss.SessionRouteJSON(ctx, "ses-child"); err != nil || route != "" {
			t.Fatalf("no route %q %v", route, err)
		}
		if _, err := ss.SessionRouteJSON(ctx, "nope"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("route of a missing session: %v", err)
		}
		if err := s.C().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
			route, err := sessionstore.SessionRouteJSONTx(ctx, tx, "ses-root")
			if err == nil && route == "" {
				err = errors.New("empty route in tx")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
