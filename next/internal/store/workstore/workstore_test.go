package workstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
	"github.com/hollis-labs/tether/next/internal/store/workstore"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

const (
	prjA    = "msg://agent/local/prj_alpha"
	prjB    = "msg://agent/local/prj_beta"
	prjDep  = "msg://agent/local/prj_old"
	agentNo = "msg://agent/local/prj_agent"
)

func newStore(s *store.Stores) (*workstore.Store, *storetest.Clock) {
	clock := storetest.NewClock(t0)
	es := eventstore.New(s)
	es.Now = clock.Now
	es.NewEventID = storetest.NewIDs("evt_").New
	ws := workstore.New(s, es)
	ws.Now = clock.Now
	return ws, clock
}

func exec(t *testing.T, h *store.Handle, q string, args ...any) {
	t.Helper()
	if err := h.WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, tx.Dialect().Rebind(q), args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seedRegistry(t *testing.T, s *store.Stores) {
	t.Helper()
	for _, r := range [][3]string{{prjA, "project", "active"}, {prjB, "project", "active"}, {prjDep, "project", "deprecated"}, {agentNo, "agent", "active"}} {
		exec(t, s.C(), `INSERT INTO registry_entries (urn, kind, display_name, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			r[0], r[1], "x", r[2], "2026-11-01T00:00:00Z", "2026-11-01T00:00:00Z")
	}
}

func seedSession(t *testing.T, s *store.Stores, id, parent, workstream string) {
	t.Helper()
	nullable := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	exec(t, s.C(), `INSERT INTO sessions (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at, parent_session_id, workstream_id)
 VALUES (?, 'l', 'proj', 'agent', 'claude', '/w', 'running', ?, ?, ?, ?)`, id, "2026-11-14T12:00:00Z", "2026-11-14T12:00:00Z", nullable(parent), nullable(workstream))
}

func sessionWorkstream(t *testing.T, s *store.Stores, id string) string {
	t.Helper()
	r := s.C().Reader()
	var ws *string
	if err := r.QueryRowContext(context.Background(), r.Dialect().Rebind(`SELECT workstream_id FROM sessions WHERE id = ?`), id).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if ws == nil {
		return ""
	}
	return *ws
}

// race runs fn on n goroutines started together and returns their results.
func race[T any](n int, fn func(i int) (T, error)) ([]T, []error) {
	out, errs := make([]T, n), make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i], errs[i] = fn(i)
		}()
	}
	close(start)
	wg.Wait()
	return out, errs
}

// warm opens n pooled connections, so racers do not stagger on connection setup.
func warm(t *testing.T, s *store.Stores, n int) {
	t.Helper()
	_, errs := race(n, func(int) (struct{}, error) {
		return struct{}{}, s.C().WriteTx(context.Background(), func(context.Context, store.Tx) error { time.Sleep(50 * time.Millisecond); return nil })
	})
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkstreams(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ws, clock := newStore(s)
		seedRegistry(t, s)
		if _, err := ws.CreateWorkstream(ctx, workstore.WorkstreamRow{ProjectURNs: []string{agentNo}}); !errors.Is(err, workstore.ErrWorkstreamProject) {
			t.Fatalf("agent as project: %v", err)
		}
		if _, err := ws.CreateWorkstream(ctx, workstore.WorkstreamRow{ProjectURNs: []string{prjDep}}); !errors.Is(err, workstore.ErrWorkstreamProject) {
			t.Fatalf("new link to a deprecated project: %v", err)
		}
		a, err := ws.CreateWorkstream(ctx, workstore.WorkstreamRow{ID: "ws-a", Name: "alpha", WorkflowID: "wf-1", ProjectURNs: []string{prjB, prjA, prjA}})
		if err != nil || a.Status != "active" || !reflect.DeepEqual(a.ProjectURNs, []string{prjA, prjB}) || a.CreatedAt != "2026-11-14T12:00:00Z" {
			t.Fatalf("create %+v %v", a, err)
		}
		// Three more in the same second: pages break the created_at tie by id.
		for _, id := range []string{"ws-b", "ws-c", "ws-d"} {
			if _, err := ws.CreateWorkstream(ctx, workstore.WorkstreamRow{ID: id}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := ws.GetWorkstream(ctx, "ws-a")
		if err != nil || got.Name != "alpha" || got.WorkflowID != "wf-1" || len(got.ProjectURNs) != 2 {
			t.Fatalf("get %+v %v", got, err)
		}
		if _, err := ws.GetWorkstream(ctx, "nope"); !errors.Is(err, workstore.ErrWorkstreamNotFound) {
			t.Fatalf("missing: %v", err)
		}
		clock.Advance(time.Minute)
		empty, closed, projects := "", "closed", []string{prjB}
		up, err := ws.UpdateWorkstream(ctx, "ws-a", workstore.WorkstreamPatch{Name: &empty, Status: &closed, ProjectURNs: &projects})
		if err != nil || up.Name != "" || up.Status != "closed" || up.WorkflowID != "wf-1" || !reflect.DeepEqual(up.ProjectURNs, projects) || up.UpdatedAt != "2026-11-14T12:01:00Z" {
			t.Fatalf("update %+v %v", up, err)
		}
		bad := "open"
		if _, err := ws.UpdateWorkstream(ctx, "ws-a", workstore.WorkstreamPatch{Status: &bad}); !errors.Is(err, workstore.ErrInvalidWorkstreamStatus) {
			t.Fatalf("bad status: %v", err)
		}
		if _, err := ws.UpdateWorkstream(ctx, "nope", workstore.WorkstreamPatch{}); !errors.Is(err, workstore.ErrWorkstreamNotFound) {
			t.Fatalf("update missing: %v", err)
		}
		// A deprecated project stays linked if it already was.
		exec(t, s.C(), `INSERT INTO workstream_projects (workstream_id, project_urn) VALUES ('ws-b', ?)`, prjDep)
		keep := []string{prjDep}
		if _, err := ws.UpdateWorkstream(ctx, "ws-b", workstore.WorkstreamPatch{ProjectURNs: &keep}); err != nil {
			t.Fatalf("keep deprecated link: %v", err)
		}

		var ids []string
		cursor := ""
		for {
			page, err := ws.ListWorkstreamsPage(ctx, workstore.ListWorkstreamsOptions{Limit: 1, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range page.Workstreams {
				ids = append(ids, w.ID)
			}
			if !page.HasMore {
				break
			}
			cursor = page.NextCursor
		}
		if !reflect.DeepEqual(ids, []string{"ws-d", "ws-c", "ws-b", "ws-a"}) {
			t.Fatalf("pages %v", ids)
		}
		for name, c := range map[string]struct {
			opts workstore.ListWorkstreamsOptions
			want int
		}{
			"status":   {workstore.ListWorkstreamsOptions{Status: "closed"}, 1},
			"workflow": {workstore.ListWorkstreamsOptions{WorkflowID: "wf-1"}, 1},
			"project":  {workstore.ListWorkstreamsOptions{ProjectURN: prjDep}, 1},
		} {
			if got, err := ws.ListWorkstreams(ctx, c.opts); err != nil || len(got) != c.want {
				t.Errorf("%s: %d %v", name, len(got), err)
			}
		}
		if _, err := ws.ListWorkstreamsPage(ctx, workstore.ListWorkstreamsOptions{Status: "closed", Cursor: cursor}); !errors.Is(err, workstore.ErrWorkstreamPage) {
			t.Fatalf("cursor for other filters: %v", err)
		}
		if _, err := ws.ListWorkstreamsPage(ctx, workstore.ListWorkstreamsOptions{Ref: "x:1", Status: "active"}); !errors.Is(err, workstore.ErrWorkstreamPage) {
			t.Fatalf("ref with filters: %v", err)
		}

		// Ref and item lookups, and the digest's session read.
		seedSession(t, s, "ses-1", "", "ws-a")
		seedSession(t, s, "ses-2", "ses-1", "ws-c")
		seedSession(t, s, "ses-0", "", "ws-a")
		for _, r := range [][3]string{{"ses-1", "torque_task", "T-1"}, {"ses-2", "torque_task", "T-1"}, {"ses-2", workstore.KindTesseractRevision, "rev-1"}} {
			exec(t, s.C(), `INSERT INTO session_refs (session_id, kind, ref_id, relation, source, at) VALUES (?, ?, ?, 'read', 'proxy', ?)`, r[0], r[1], r[2], "2026-11-14T12:00:00Z")
		}
		if got, err := ws.ListWorkstreams(ctx, workstore.ListWorkstreamsOptions{Ref: "torque_task:T-1"}); err != nil || len(got) != 2 || got[0].ID != "ws-c" {
			t.Fatalf("ref page %+v %v", got, err)
		}
		if got, err := ws.WorkstreamsForRef(ctx, "torque_task", "T-1"); err != nil || len(got) != 2 || got[0].ID != "ws-c" || got[1].ID != "ws-a" {
			t.Fatalf("for ref %+v %v", got, err)
		}
		if got, err := ws.WorkstreamsForItem(ctx, "item-1", "rev-1"); err != nil || len(got) != 1 || got[0].ID != "ws-c" {
			t.Fatalf("for item %+v %v", got, err)
		}
		if got, err := ws.SessionsInWorkstream(ctx, "ws-a"); err != nil || len(got) != 2 || got[0].ID != "ses-0" || got[1].WorkstreamID != "ws-a" {
			t.Fatalf("sessions in workstream %+v %v", got, err)
		}
	})
}

func TestAssignEnsureAndNamespace(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ws, _ := newStore(s)
		seedSession(t, s, "root", "", "")
		seedSession(t, s, "mid", "root", "")
		seedSession(t, s, "leaf", "mid", "")
		seedSession(t, s, "sib", "mid", "")
		if err := ws.AssignSessionWorkstream(ctx, "leaf", "nope"); !errors.Is(err, workstore.ErrWorkstreamNotFound) {
			t.Fatalf("assign unknown workstream: %v", err)
		}
		if _, err := ws.CreateWorkstream(ctx, workstore.WorkstreamRow{ID: "ws-x"}); err != nil {
			t.Fatal(err)
		}
		if err := ws.AssignSessionWorkstream(ctx, "ghost", "ws-x"); !errors.Is(err, workstore.ErrSessionNotFound) {
			t.Fatalf("assign unknown session: %v", err)
		}
		got, err := ws.EnsureSessionWorkstream(ctx, "leaf", workstore.WorkstreamRow{Name: "seeded"})
		if err != nil || got.Name != "seeded" {
			t.Fatalf("ensure %+v %v", got, err)
		}
		for _, id := range []string{"leaf", "mid", "root"} {
			if w := sessionWorkstream(t, s, id); w != got.ID {
				t.Fatalf("%s in %q, want %q", id, w, got.ID)
			}
		}
		if again, err := ws.EnsureSessionWorkstream(ctx, "sib", workstore.WorkstreamRow{}); err != nil || again.ID != got.ID {
			t.Fatalf("sibling converges: %+v %v", again, err)
		}
		// A session placed elsewhere is never moved.
		if err := ws.AssignSessionWorkstream(ctx, "mid", "ws-x"); err != nil {
			t.Fatal(err)
		}
		seedSession(t, s, "leaf2", "mid", "")
		if near, err := ws.EnsureSessionWorkstream(ctx, "leaf2", workstore.WorkstreamRow{}); err != nil || near.ID != "ws-x" || sessionWorkstream(t, s, "root") != got.ID {
			t.Fatalf("nearest wins, never moved: %+v %v", near, err)
		}
		if err := ws.AssignSessionWorkstream(ctx, "leaf2", ""); err != nil || sessionWorkstream(t, s, "leaf2") != "" {
			t.Fatalf("clear: %v", err)
		}
		if _, err := ws.EnsureSessionWorkstream(ctx, "ghost", workstore.WorkstreamRow{}); !errors.Is(err, workstore.ErrSessionNotFound) {
			t.Fatalf("ensure unknown: %v", err)
		}
		ns, err := ws.SessionWorkstreamNamespace(ctx, "leaf")
		if err != nil || ns.Namespace != "project/proj/workspace/scratch" || ns.WorkstreamID != got.ID {
			t.Fatalf("namespace %+v %v", ns, err)
		}
		if ns, err := ws.WorkstreamTarget(ctx, got.ID, workstore.WorkstreamTargetOptions{Owner: "/team/", Tail: "/notes/"}); err != nil || ns.Namespace != "team/workspace/notes" {
			t.Fatalf("target %+v %v", ns, err)
		}
		if _, err := ws.WorkstreamTarget(ctx, "leaf"); !errors.Is(err, workstore.ErrNotAWorkstream) {
			t.Fatalf("session id as target: %v", err)
		}
		if _, err := ws.SessionWorkstreamNamespace(ctx, "leaf2"); !errors.Is(err, workstore.ErrWorkstreamNotFound) {
			t.Fatalf("no workstream: %v", err)
		}
	})
}

// TestEnsureConcurrent: ensures from sibling leaves of one parentless lineage race; every one
// returns the same workstream, and only one is created.
func TestEnsureConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ws, _ := newStore(s)
		seedSession(t, s, "root", "", "")
		const n = 6
		for i := range n {
			seedSession(t, s, fmt.Sprint("leaf-", i), "root", "")
		}
		warm(t, s, n)
		got, errs := race(n, func(i int) (string, error) {
			w, err := ws.EnsureSessionWorkstream(ctx, fmt.Sprint("leaf-", i), workstore.WorkstreamRow{})
			return w.ID, err
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("ensure %d: %v", i, err)
			}
			if got[i] != got[0] {
				t.Fatalf("ensures returned %v", got)
			}
		}
		if all, _ := ws.ListWorkstreams(ctx, workstore.ListWorkstreamsOptions{}); len(all) != 1 {
			t.Fatalf("%d workstreams created, want 1", len(all))
		}
	})
}

func request(t *testing.T, ws *workstore.Store, session string, req workstore.EnvironmentRequest) (eventstore.Row, error) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return ws.RecordEnvironmentRequest(context.Background(), eventstore.Row{Scope: "session", SessionID: session, PayloadJSON: string(raw)})
}

func TestEnvironmentRequestsAndWorkState(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ws, clock := newStore(s)
		seedSession(t, s, "ses-1", "", "")
		for _, bad := range []workstore.EnvironmentRequest{
			{TurnID: "t1", Open: true, RequestID: "r", Kind: "other", SourceSequence: 1},
			{RequestID: "r", SourceSequence: 1},
			{Terminal: true, TurnID: "t1"},
		} {
			if _, err := request(t, ws, "ses-1", bad); !errors.Is(err, workstore.ErrEnvironmentRequestIdentity) {
				t.Fatalf("%+v: %v", bad, err)
			}
		}
		ev, err := request(t, ws, "ses-1", workstore.EnvironmentRequest{TurnID: "t1", RequestID: "q1", Kind: "question", Open: true, SourceSequence: 5})
		if err != nil || ev.Kind != workstore.EnvironmentRequestKind || ev.EventID == "" {
			t.Fatalf("open %+v %v", ev, err)
		}
		snap, err := ws.EnvironmentSnapshot(ctx, "ses-1")
		if err != nil || len(snap.Requests) != 1 || !snap.Requests[0].Open || snap.HighWater != ev.Seq || len(snap.Sessions) != 1 || snap.Sessions[0].LastEventAt == "" {
			t.Fatalf("snapshot %+v %v", snap, err)
		}
		if _, err := request(t, ws, "ses-1", workstore.EnvironmentRequest{TurnID: "t1", RequestID: "q1", SourceSequence: 6}); err != nil {
			t.Fatal(err)
		}
		// A late open below the resolution is stored closed, and its event says so.
		late, err := request(t, ws, "ses-1", workstore.EnvironmentRequest{TurnID: "t1", RequestID: "q1", Kind: "question", Open: true, SourceSequence: 4})
		if err != nil {
			t.Fatal(err)
		}
		var payload workstore.EnvironmentRequest
		if err := json.Unmarshal([]byte(late.PayloadJSON), &payload); err != nil || payload.Open || payload.SourceSequence != 6 {
			t.Fatalf("late payload %s %v", late.PayloadJSON, err)
		}
		if _, err := request(t, ws, "ses-1", workstore.EnvironmentRequest{TurnID: "t2", RequestID: "a1", Kind: "approval", Open: true, SourceSequence: 7}); err != nil {
			t.Fatal(err)
		}
		if _, err := request(t, ws, "ses-1", workstore.EnvironmentRequest{TurnID: "t2", Terminal: true, SourceSequence: 9}); err != nil {
			t.Fatal(err)
		}
		snap, _ = ws.EnvironmentSnapshot(ctx, "")
		if len(snap.Requests) != 2 || snap.Requests[0].Open || snap.Requests[1].Open || snap.Requests[1].SourceSequence != 9 {
			t.Fatalf("requests %+v", snap.Requests)
		}

		w1, err := ws.RecordWorkState(ctx, eventstore.Row{Scope: "session", SessionID: "ses-1", PayloadJSON: `{"to":"processing","at":"2026-11-14T12:00:01Z"}`})
		if err != nil || w1.Kind != workstore.WorkStateKind {
			t.Fatalf("work state %+v %v", w1, err)
		}
		clock.Advance(time.Second)
		if _, err := ws.RecordWorkState(ctx, eventstore.Row{Scope: "session", SessionID: "ses-1", PayloadJSON: `{"to":"bogus"}`}); err != nil {
			t.Fatal(err)
		}
		if cur, ok, err := ws.LatestWorkState(ctx, "ses-1"); err != nil || !ok || cur.Value != "processing" {
			t.Fatalf("latest %+v %v %v", cur, ok, err)
		}
		if _, err := ws.RecordWorkState(ctx, eventstore.Row{Scope: "session", SessionID: "ses-1", PayloadJSON: `{"to":"idle"}`}); err != nil {
			t.Fatal(err)
		}
		snap, _ = ws.EnvironmentSnapshot(ctx, "ses-1")
		if snap.WorkStates["ses-1"].Value != "idle" || !snap.WorkStates["ses-1"].At.Equal(clock.Now()) {
			t.Fatalf("work states %+v", snap.WorkStates)
		}
		if _, ok, _ := ws.LatestWorkState(ctx, "nobody"); ok {
			t.Fatal("work state for an unknown session")
		}
	})
}

// TestEnvironmentRequestRace: an open and the resolution of the same request race, for many
// requests at once. Whatever the order, the request must end closed: on Postgres the (session, turn)
// lock serializes the fence read and the status write.
func TestEnvironmentRequestRace(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ws, _ := newStore(s)
		const n = 12
		warm(t, s, 2*n)
		_, errs := race(2*n, func(i int) (struct{}, error) {
			req := workstore.EnvironmentRequest{TurnID: "t", RequestID: fmt.Sprint("q", i/2), SourceSequence: 5}
			if i%2 == 0 {
				req.Kind, req.Open = "question", true
			} else {
				req.SourceSequence = 6
			}
			_, err := request(t, ws, "ses-r", req)
			return struct{}{}, err
		})
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		snap, err := ws.EnvironmentSnapshot(ctx, "ses-r")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range snap.Requests {
			if r.Open {
				t.Fatalf("request %s left open after its resolution", r.RequestID)
			}
		}
	})
}

func TestEnvironmentEvents(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ws, _ := newStore(s)
		if w, err := ws.EnvironmentEvents(ctx, 0, 10); err != nil || w.HighWater != 0 || w.GapReason != "" || len(w.Events) != 0 {
			t.Fatalf("empty %+v %v", w, err)
		}
		var seqs []int64
		for range 4 {
			r, err := ws.Events.Insert(ctx, eventstore.Row{Scope: "global", Kind: "k"})
			if err != nil {
				t.Fatal(err)
			}
			seqs = append(seqs, r.Seq)
		}
		head := seqs[3]
		if w, _ := ws.EnvironmentEvents(ctx, seqs[0], 10); len(w.Events) != 3 || w.Events[0].Seq != seqs[1] || w.HighWater != head {
			t.Fatalf("window %+v", w)
		}
		if w, _ := ws.EnvironmentEvents(ctx, seqs[0], 2); w.GapReason != "too_large" || w.Events != nil {
			t.Fatalf("too large %+v", w)
		}
		if w, _ := ws.EnvironmentEvents(ctx, head+5, 10); w.GapReason != "ahead" {
			t.Fatalf("ahead %+v", w)
		}
		if w, _ := ws.EnvironmentEvents(ctx, head, 10); w.GapReason != "" || len(w.Events) != 0 {
			t.Fatalf("at head %+v", w)
		}
		exec(t, s.B(), `DELETE FROM events WHERE id = ?`, seqs[2])
		if w, _ := ws.EnvironmentEvents(ctx, seqs[0], 10); w.GapReason != "purged" || w.Events != nil {
			t.Fatalf("hole %+v", w)
		}
		exec(t, s.B(), `DELETE FROM events WHERE id IN (?, ?)`, seqs[0], seqs[1])
		if w, _ := ws.EnvironmentEvents(ctx, 0, 10); w.GapReason != "purged" || w.EarliestAvailable != head {
			t.Fatalf("purged prefix %+v", w)
		}
	})
}
