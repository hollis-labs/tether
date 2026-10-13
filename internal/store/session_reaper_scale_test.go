package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
)

// seedNoisySessions mimics the hub that ran away (CW-20261012-0120): many detached hub-local sessions, each
// owning thousands of noise events (output chunks, shim_status re-assertions) and only a handful of
// events that mean anything.
func seedNoisySessions(t *testing.T, db *Store, sessions, noisePer int, activityAt time.Time) {
	t.Helper()
	for i := 0; i < sessions; i++ {
		id := fmt.Sprintf("dead-%03d", i)
		if err := db.CreateSession(SessionRow{ID: id, State: "detached"}, &launch.Plan{}); err != nil {
			t.Fatal(err)
		}
		tx, err := db.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		base := activityAt.Add(-48 * time.Hour)
		stmt, err := tx.Prepare(`INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES('session',?,?,?,?)`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = stmt.Exec(id, base.Format(time.RFC3339Nano), "session.state_changed", `{"from":"created","to":"running"}`); err != nil {
			t.Fatal(err)
		}
		if _, err = stmt.Exec(id, activityAt.Add(-24*time.Hour).Format(time.RFC3339Nano), "session.activity", fmt.Sprintf(`{"at":%q}`, activityAt.Format(time.RFC3339Nano))); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < noisePer; n++ {
			// Noise lands AFTER the real activity: it must never move LastActivity.
			at := activityAt.Add(time.Duration(n+1) * time.Second).Format(time.RFC3339Nano)
			kind := "session.output"
			if n%2 == 0 {
				kind = "session.shim_status"
			}
			if _, err = stmt.Exec(id, at, kind, `{"state":"detached","reason":"outcome_unknown"}`); err != nil {
				t.Fatal(err)
			}
		}
		_ = stmt.Close()
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReaperTimesIgnoreNoiseAndStayCheapAtScale(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	activity := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	const sessions, noise = 40, 4000
	seedNoisySessions(t, db, sessions, noise, activity)

	began := time.Now()
	rows, err := db.ReaperSessions(context.Background())
	took := time.Since(began)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != sessions {
		t.Fatalf("rows=%d", len(rows))
	}
	for _, r := range rows {
		// The newest meaningful event is the recorded session.activity, never the later noise.
		if !r.LastActivity.Equal(activity) {
			t.Fatalf("%s: last activity %s, want %s (noise counted as activity)", r.ID, r.LastActivity, activity)
		}
		if want := activity.Add(-48 * time.Hour); !r.StartedAt.Equal(want) {
			t.Fatalf("%s: started %s, want %s", r.ID, r.StartedAt, want)
		}
	}
	// 160k noise rows. Indexed lookups finish in milliseconds; the old per-session JSON aggregates over
	// every event took seconds. The bound is deliberately generous so slow CI does not flake, but it is
	// two orders of magnitude below the old cost on this data.
	if took > 3*time.Second {
		t.Fatalf("ReaperSessions over %d noisy events took %s", sessions*noise, took)
	}
}

func TestReaperAndShimLookupsUseTheSessionKindIndex(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`SELECT at,payload_json FROM events WHERE session_id=? AND kind='session.state_changed' ORDER BY at ASC,id ASC LIMIT 256`,
		`SELECT at,payload_json FROM events WHERE session_id=? AND kind=? ORDER BY at DESC,id DESC LIMIT 1`,
	} {
		args := []any{"x"}
		if strings.Contains(q, "kind=?") {
			args = append(args, "session.shim_status")
		}
		rows, err := db.DB().Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		joined := strings.Join(plan, " | ")
		if !strings.Contains(joined, "idx_events_session_kind_at") || strings.Contains(joined, "TEMP B-TREE") {
			t.Fatalf("query is not served from idx_events_session_kind_at without a sort: %s\n%s", q, joined)
		}
	}
}

func TestLatestSessionEventReturnsNewestOfKind(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(SessionRow{ID: "s", State: "detached"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if e, err := db.LatestSessionEvent(context.Background(), "s", "session.shim_status"); err != nil || e != nil {
		t.Fatalf("empty: %v %v", e, err)
	}
	t0 := time.Now().UTC()
	for i, reason := range []string{"a", "b", "c"} {
		if _, err := db.DB().Exec(`INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES('session','s',?,?,?)`, t0.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), "session.shim_status", fmt.Sprintf(`{"reason":%q}`, reason)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().Exec(`INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES('session','s',?,?,?)`, t0.Add(time.Hour).Format(time.RFC3339Nano), "session.output", `{}`); err != nil {
		t.Fatal(err)
	}
	e, err := db.LatestSessionEvent(context.Background(), "s", "session.shim_status")
	if err != nil || e == nil || !strings.Contains(e.PayloadJSON, `"c"`) {
		t.Fatalf("latest=%+v err=%v", e, err)
	}
}
