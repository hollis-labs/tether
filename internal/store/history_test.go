package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// historyStore opens a store with n events alternating kinds a and b.
func historyStore(t *testing.T, n int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for i := range n {
		kind := "a"
		if i%2 == 1 {
			kind = "b"
		}
		if _, _, err = s.InsertEvent(events.ScopeDaemon, "", kind, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func smallWindows(t *testing.T) {
	t.Helper()
	oldMin, oldMax := historyWindowMin, historyWindowMax
	historyWindowMin, historyWindowMax = 4, 16
	t.Cleanup(func() { historyWindowMin, historyWindowMax = oldMin, oldMax })
}

func seqs(evs []events.Event) []int64 {
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

// CW-20261114-0166: a kind-filtered newest-first history read is served by
// idx_events_kind_id, not a scan of the whole table.
func TestKindHistoryReadUsesTheIndex(t *testing.T) {
	s := historyStore(t, 4)
	query, args := historyQuery(EventFilter{Kinds: []string{"a"}})
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+query, append(args, 1, 100, 10)...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "idx_events_kind_id") {
		t.Fatalf("plan does not use idx_events_kind_id:\n%s", strings.Join(plan, "\n"))
	}
}

// Paging through history window by window gives exactly what the plain query gives.
func TestHistoryPagesMatchTheQuery(t *testing.T) {
	smallWindows(t)
	s := historyStore(t, 61)
	for _, f := range []EventFilter{{Kinds: []string{"a"}, Limit: 7}, {Kinds: []string{"b"}, Limit: 100}, {Limit: 9}, {Kinds: []string{"a"}, SinceSeq: 20, Limit: 4}} {
		want, err := s.QueryEvents(EventFilter{Kinds: f.Kinds, SinceSeq: f.SinceSeq, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		var got []events.Event
		for page := 0; ; page++ {
			p, err := s.QueryEventHistory(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			if p.Partial || len(p.Events) > f.Limit {
				t.Fatalf("page %d of %+v: partial %v, %d events", page, f, p.Partial, len(p.Events))
			}
			got = append(got, p.Events...)
			if p.NextCursor == 0 {
				break
			}
			f.Cursor = p.NextCursor
		}
		if !slices.Equal(seqs(got), seqs(want)) {
			t.Fatalf("%+v: history %v, query %v", f, seqs(got), seqs(want))
		}
	}
}

// A read that runs out of time returns what it found and a cursor at the edge
// of what it searched; continuing from it loses and repeats nothing.
func TestHistoryDeadlineReturnsAPartialPageAndCursor(t *testing.T) {
	smallWindows(t)
	s := historyStore(t, 61)
	want, err := s.QueryEvents(EventFilter{Kinds: []string{"b"}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	afterHistoryWindow = cancel // out of time after the first window
	t.Cleanup(func() { afterHistoryWindow = nil })
	p, err := s.QueryEventHistory(ctx, EventFilter{Kinds: []string{"b"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Partial || p.NextCursor == 0 || len(p.Events) == 0 || len(p.Events) >= len(want) {
		t.Fatalf("partial page: partial %v cursor %d events %v", p.Partial, p.NextCursor, seqs(p.Events))
	}
	afterHistoryWindow = nil
	rest, err := s.QueryEventHistory(context.Background(), EventFilter{Kinds: []string{"b"}, Limit: 100, Cursor: p.NextCursor})
	if err != nil || rest.Partial {
		t.Fatalf("continue: %v partial %v", err, rest.Partial)
	}
	if got := append(seqs(p.Events), seqs(rest.Events)...); !slices.Equal(got, seqs(want)) {
		t.Fatalf("partial + rest = %v, want %v", got, seqs(want))
	}

	// Already out of time: no error, a partial page.
	done, stop := context.WithCancel(context.Background())
	stop()
	if p, err = s.QueryEventHistory(done, EventFilter{Kinds: []string{"b"}}); err != nil || !p.Partial {
		t.Fatalf("expired context: %v partial %v", err, p.Partial)
	}
}

// History reads run on the read-only pool: they answer while the writer's only
// connection is held, as it was by the 2-minute scan.
func TestHistoryReadsDoNotNeedTheWriter(t *testing.T) {
	s := historyStore(t, 10)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p, err := s.QueryEventHistory(ctx, EventFilter{Kinds: []string{"a"}})
	if err != nil || p.Partial || len(p.Events) != 5 {
		t.Fatalf("read with the writer held: %v partial %v events %d", err, p.Partial, len(p.Events))
	}
	if _, err = s.reader.Exec(`INSERT INTO events(scope, session_id, at, kind) VALUES('daemon','','now','x')`); err == nil {
		t.Fatal("the history pool accepted a write")
	}
}

// /health's WAL block: size, and how long the checkpoint sequence has stood still.
func TestWALStatusTracksCheckpoints(t *testing.T) {
	s := historyStore(t, 20)
	t0 := time.Now()
	st, err := s.WALStatus(t0)
	if err != nil {
		t.Fatal(err)
	}
	if st.WALBytes == 0 || st.LastCheckpointObserved || st.LastCheckpointAgeSec != 0 {
		t.Fatalf("first sample: %+v", st)
	}
	if st, _ = s.WALStatus(t0.Add(10 * time.Minute)); st.LastCheckpointAgeSec != 600 {
		t.Fatalf("no checkpoint for 10m: %+v", st)
	}
	seq := st.CheckpointSeq
	// A full checkpoint, then a write: SQLite restarts the log and advances the sequence.
	if _, err = s.db.Exec(`PRAGMA wal_checkpoint(RESTART)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.InsertEvent(events.ScopeDaemon, "", "a", `{}`); err != nil {
		t.Fatal(err)
	}
	st, _ = s.WALStatus(t0.Add(11 * time.Minute))
	if st.CheckpointSeq == seq || !st.LastCheckpointObserved || st.LastCheckpointAgeSec != 0 {
		t.Fatalf("after a checkpoint: %+v (seq was %d)", st, seq)
	}
	if st, _ = s.WALStatus(t0.Add(14 * time.Minute)); st.LastCheckpointAgeSec != 180 {
		t.Fatalf("3m after the checkpoint: %+v", st)
	}
}

// planFor is the EXPLAIN QUERY PLAN detail of query, one step per line.
func planFor(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, "\n")
}

// #579 review: idx_events_kind_id saves a sort, so without a session+kind+id
// index the planner chose it for session_id=? AND kind=? queries ordered or
// maxed by id and walked every row of the kind (3.3 s for one old session on
// the hub). Every such shape the store uses must plan on the session index.
func TestSessionKindQueriesUseTheSessionIndex(t *testing.T) {
	s := historyStore(t, 4)
	for _, q := range []string{
		`SELECT id FROM events WHERE session_id=? AND kind=? ORDER BY id DESC LIMIT 100`,
		`SELECT id FROM events WHERE session_id=? AND kind=? ORDER BY id LIMIT 1025`,
		`SELECT MAX(id) FROM events WHERE session_id=? AND kind=?`,
		`SELECT COUNT(*) FROM events WHERE session_id=? AND kind=? AND id > ?`,
	} {
		args := []any{"s", "session.output"}
		if strings.Count(q, "?") == 3 {
			args = append(args, 0)
		}
		if plan := planFor(t, s, q, args...); !strings.Contains(plan, "idx_events_session_kind_id") {
			t.Errorf("%s\nplans as:\n%s", q, plan)
		}
	}
	query, args := historyQuery(EventFilter{Kinds: []string{"a"}})
	if plan := planFor(t, s, query, append(args, 1, 100, 10)...); !strings.Contains(plan, "idx_events_kind_id") {
		t.Errorf("kind history plans as:\n%s", plan)
	}
}

// An old session's events are found, whole and within the deadline, however
// many newer events other sessions have written since (#579 review).
func TestOldSessionHistoryIsFoundWithinTheDeadline(t *testing.T) {
	smallWindows(t)
	s := historyStore(t, 0)
	for i := range 6 {
		if _, _, err := s.InsertEvent(events.ScopeSession, "old", []string{"session.output", "session.state_changed"}[i%2], `{}`); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		if _, err = tx.Exec(`INSERT INTO events(scope, session_id, at, kind) VALUES('session', ?, 'now', 'session.output')`, fmt.Sprintf("new-%d", i%50)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	windows := 0
	afterHistoryWindow = func() { windows++ }
	t.Cleanup(func() { afterHistoryWindow = nil })
	for _, f := range []EventFilter{{SessionID: "old"}, {SessionID: "old", Kinds: []string{"session.output"}}} {
		ctx, cancel := context.WithTimeout(context.Background(), HistoryDeadline)
		start := time.Now()
		p, err := s.QueryEventHistory(ctx, f)
		cancel()
		want := 6
		if len(f.Kinds) > 0 {
			want = 3
		}
		if err != nil || p.Partial || len(p.Events) != want || p.NextCursor != 0 {
			t.Fatalf("%+v: err %v partial %v events %d cursor %d after %s", f, err, p.Partial, len(p.Events), p.NextCursor, time.Since(start))
		}
	}
	if windows != 0 {
		t.Fatalf("a session read scanned %d id windows; it should use the session index", windows)
	}
}
