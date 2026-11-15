package sessionstore_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// repRace runs fn on n goroutines at once and returns their results.
func repRace[T any](n int, fn func() (T, error)) ([]T, []error) {
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

// repWarm opens n pooled connections first, so the racers overlap instead of staggering on
// connection setup.
func repWarm(s *store.Stores, n int) {
	repRace(n, func() (struct{}, error) {
		return struct{}{}, s.C().WriteTx(context.Background(), func(context.Context, store.Tx) error { time.Sleep(50 * time.Millisecond); return nil })
	})
}

func repExec(t *testing.T, h *store.Handle, q string, args ...any) {
	t.Helper()
	if err := h.WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, tx.Dialect().Rebind(q), args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func repState(t *testing.T, ss *sessionstore.Store, id string) sessionstore.SessionRow {
	t.Helper()
	got, err := ss.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// repRunning creates a running session with pid, a session principal and a binding.
func repRunning(t *testing.T, s *store.Stores, ss *sessionstore.Store, id string, pid int) {
	t.Helper()
	ctx := context.Background()
	r := row(id)
	if _, err := ss.CreateSession(ctx, r, `{"plan":1}`, nil); err != nil {
		t.Fatal(err)
	}
	if err := ss.UpdateSessionState(ctx, id, "running", pid, nil); err != nil {
		t.Fatal(err)
	}
	repExec(t, s.A(), `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json, created_at)
 VALUES (?, 'session', 'd', ?, '["*"]', ?, '[]', '2026-11-14T00:00:00Z')`, "msg://session/local/"+id, repHash(id), id)
	repExec(t, s.C(), `INSERT INTO runtime_bindings (id, target_urn, session_id, host_id, attempt_id, generation, leased_at, created_at, updated_at)
 VALUES (?, ?, ?, 'local', ?, 1, '2026-11-14T00:00:00Z', '2026-11-14T00:00:00Z', '2026-11-14T00:00:00Z')`, "bind-"+id, "msg://agent/x/"+id, id, "att-"+id)
}

func repHash(seed string) string {
	h := []byte("0000000000000000000000000000000000000000000000000000000000000000")
	copy(h, seed)
	return string(h)
}

func repRevoked(t *testing.T, s *store.Stores, id string) (principal, binding bool) {
	t.Helper()
	ctx := context.Background()
	var p, b sql.NullString
	if err := s.A().Reader().QueryRowContext(ctx, s.A().Dialect().Rebind(`SELECT revoked_at FROM principals WHERE session_id = ?`), id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if err := s.C().Reader().QueryRowContext(ctx, s.C().Dialect().Rebind(`SELECT revoked_at FROM runtime_bindings WHERE session_id = ?`), id).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return p.Valid, b.Valid
}

func TestRecoveryStateChanges(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repRunning(t, s, ss, "ses-1", 42)
		if _, err := ss.MarkSessionDetached(ctx, "ses-1", ""); err == nil {
			t.Fatal("a recovery without a reason was accepted")
		}
		change, err := ss.MarkSessionDetached(ctx, "ses-1", "socket lost")
		if err != nil || change.From != "running" || change.To != "detached" || change.LogicalAgentID != "agent" {
			t.Fatalf("detach %+v %v", change, err)
		}
		if got := repState(t, ss, "ses-1"); got.PID == nil || *got.PID != 42 {
			t.Fatalf("detach dropped the pid: %+v", got)
		}
		if p, b := repRevoked(t, s, "ses-1"); p || b {
			t.Fatal("detach revoked authority")
		}
		if id, _ := ss.DetachedSessionIDForAgent(ctx, "agent"); id != "ses-1" {
			t.Fatalf("detached for agent %q", id)
		}
		if _, err := ss.MarkSessionOrphaned(ctx, "ses-1", "process gone"); err != nil {
			t.Fatal(err)
		}
		got := repState(t, ss, "ses-1")
		if got.State != "orphaned" || got.PID != nil || got.EndedAt != "" {
			t.Fatalf("orphaned row %+v", got)
		}
		if p, b := repRevoked(t, s, "ses-1"); !p || !b {
			t.Fatalf("orphaning left authority: principal revoked %v binding revoked %v", p, b)
		}
		if _, err := ss.MarkSessionDetached(ctx, "ses-1", "late"); err == nil {
			t.Fatal("orphaned moved back to detached")
		}
		if _, err := ss.MarkSessionOrphaned(ctx, "ses-1", "again"); err != nil {
			t.Fatalf("re-orphan: %v", err)
		}
		if _, err := ss.MarkSessionOrphaned(ctx, "nope", "r"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("missing session: %v", err)
		}
		if id, _ := ss.DetachedSessionIDForAgent(ctx, "agent"); id != "" {
			t.Fatalf("detached for agent after orphaning %q", id)
		}
	})
}

// TestRecoveryStateConcurrent races one orphaning against detaches of the same running session.
// Orphaned cannot go back to detached, so whatever the order the session must end orphaned, with its
// process identity cleared; without the row lock a detach that read "running" before the orphaning
// committed would overwrite it on Postgres.
func TestRecoveryStateConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		for round := range 4 {
			id := "ses-race-" + string(rune('a'+round))
			repRunning(t, s, ss, id, 7)
			repWarm(s, 6)
			var n atomic.Int32
			repRace(6, func() (sessionstore.SessionStateChange, error) {
				if n.Add(1) == 1 {
					return ss.MarkSessionOrphaned(ctx, id, "gone")
				}
				return ss.MarkSessionDetached(ctx, id, "lost")
			})
			if got := repState(t, ss, id); got.State != "orphaned" || got.PID != nil {
				t.Fatalf("round %d: session %+v", round, got)
			}
		}
	})
}

func TestReaper(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		repRunning(t, s, ss, "ses-1", 42)
		clock.Advance(time.Second)
		repRunning(t, s, ss, "ses-2", 43)
		repExec(t, s.C(), `UPDATE sessions SET worker_id = 'w1' WHERE id = 'ses-2'`)
		cands, err := ss.ReaperCandidates(ctx)
		if err != nil || len(cands) != 1 || cands[0].ID != "ses-1" || !cands[0].StartedAt.Equal(t0) {
			t.Fatalf("candidates %+v %v", cands, err)
		}
		insert := func(kind, payload string, at time.Time) eventstore.Row {
			t.Helper()
			r, err := es.Insert(ctx, eventstore.Row{Scope: "session", SessionID: "ses-1", Kind: kind, PayloadJSON: payload, At: at})
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		insert("session.state_changed", `{"to":"created"}`, t0.Add(time.Second))
		insert("session.state_changed", `{"to":"running"}`, t0.Add(2*time.Second))
		insert("session.output", `{}`, t0.Add(time.Hour)) // noise: not activity
		insert("tool_call_end", `{}`, t0.Add(3*time.Second))
		insert("session.activity", `{"at":"2026-11-14T12:00:09Z"}`, t0.Add(5*time.Second))
		c := cands[0]
		if err := ss.ReaperTimes(ctx, &c); err != nil {
			t.Fatal(err)
		}
		if !c.StartedAt.Equal(t0.Add(2*time.Second)) || !c.LastActivity.Equal(t0.Add(9*time.Second)) {
			t.Fatalf("times started %v activity %v", c.StartedAt, c.LastActivity)
		}
		latest, err := ss.LatestSessionEvent(ctx, es, "ses-1", "session.state_changed")
		if err != nil || latest == nil || latest.PayloadJSON != `{"to":"running"}` {
			t.Fatalf("latest %+v %v", latest, err)
		}
		if none, err := ss.LatestSessionEvent(ctx, es, "ses-1", "nothing"); err != nil || none != nil {
			t.Fatalf("latest of a missing kind %+v %v", none, err)
		}

		// A stale snapshot loses to the session moving on.
		stale := c.SessionRow
		stale.UpdatedAt = "2000-01-01T00:00:00Z"
		if ok, err := ss.RecordReaperOrphan(ctx, es, "ses-1", "stale", &stale, nil); err != nil || ok {
			t.Fatalf("stale snapshot orphaned: %v %v", ok, err)
		}
		before, _ := es.Pending(ctx, 0, 0)
		ok, err := ss.RecordReaperOrphan(ctx, es, "ses-1", "silent", &c.SessionRow, func(r eventstore.Row) eventstore.Row { r.ProjectID = "proj"; return r })
		if err != nil || !ok {
			t.Fatalf("orphan %v %v", ok, err)
		}
		if got := repState(t, ss, "ses-1"); got.State != "orphaned" || got.PID != nil {
			t.Fatalf("orphaned %+v", got)
		}
		if p, b := repRevoked(t, s, "ses-1"); !p || !b {
			t.Fatal("reaper left authority")
		}
		after, _ := es.Pending(ctx, 0, 0)
		if len(after) != len(before)+1 {
			t.Fatalf("outbox rows %d -> %d", len(before), len(after))
		}
		last := after[len(after)-1]
		if last.Kind != "session.reaper" || last.ProjectID != "proj" || last.PayloadJSON != `{"outcome":"orphaned","reason":"silent","stage":"outcome"}` {
			t.Fatalf("reaper event %+v", last)
		}
		if ok, _ := ss.RecordReaperOrphan(ctx, es, "ses-1", "again", nil, nil); ok {
			t.Fatal("an orphaned session was orphaned again")
		}
	})
}

func TestSessionLeaseExpiredAndWorkerOutput(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		if exp, err := ss.SessionLeaseExpired(ctx, "ses-1", t0); err != nil || exp {
			t.Fatalf("no binding: %v %v", exp, err)
		}
		bind := func(id, target string, gen int, lease any, revoked any) {
			repExec(t, s.C(), `INSERT INTO runtime_bindings (id, target_urn, session_id, host_id, attempt_id, generation, leased_at, lease_expires_at, revoked_at, created_at, updated_at)
 VALUES (?, ?, 'ses-1', 'local', ?, ?, 'x', ?, ?, 'x', 'x')`, id, target, "att-"+id, gen, lease, revoked)
		}
		bind("b1", "t1", 1, "2026-11-14T11:00:00Z", nil)
		if exp, _ := ss.SessionLeaseExpired(ctx, "ses-1", t0); !exp {
			t.Fatal("an expired lease is live")
		}
		bind("b2", "t2", 1, "2026-11-14 13:00:00", nil) // julianday's no-zone form
		if exp, _ := ss.SessionLeaseExpired(ctx, "ses-1", t0); exp {
			t.Fatal("a live lease did not count")
		}
		bind("b3", "t2", 2, "2026-11-14T11:30:00.5Z", nil) // supersedes b2's generation
		if exp, _ := ss.SessionLeaseExpired(ctx, "ses-1", t0); !exp {
			t.Fatal("a superseded generation still counts")
		}
		bind("b4", "t3", 1, nil, nil) // no lease bound: authority to continue
		if exp, _ := ss.SessionLeaseExpired(ctx, "ses-1", t0); exp {
			t.Fatal("an unbounded lease did not count as live")
		}

		for seq := range int64(3) {
			if err := ss.RecordWorkerOutput(ctx, es, "ses-1", seq+1, `{"frame":1}`, t0, nil); err != nil {
				t.Fatal(err)
			}
		}
		var last int64
		if err := s.C().Reader().QueryRowContext(ctx, `SELECT last_seq FROM session_worker_seq WHERE session_id = 'ses-1'`).Scan(&last); err != nil || last != 3 {
			t.Fatalf("worker seq %d %v", last, err)
		}
		if pending, _ := es.Pending(ctx, 0, 0); len(pending) != 3 || pending[0].Kind != "session.output" {
			t.Fatalf("worker output events %+v", pending)
		}
	})
}

func TestTeamEvidence(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		if _, err := ss.TeamEvidence(ctx, "k", "e"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing evidence: %v", err)
		}
		for _, p := range []string{`{"first":true}`, `{"second":true}`} {
			if err := ss.RecordTeamEvidence(ctx, "k", "e", p); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := ss.TeamEvidence(ctx, "k", "e"); err != nil || got != `{"first":true}` {
			t.Fatalf("evidence %q %v", got, err)
		}
	})
}
