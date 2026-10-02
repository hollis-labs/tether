package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/hollis-labs/tether/internal/callcontext"
)

func TestEventHistoryRetentionAuditAndRollback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-90 * 24 * time.Hour)
	for _, table := range []string{"events", "proxy_events", "ai_events", "identity_audit", "a2a_tasks"} {
		t.Run(table, func(t *testing.T) {
			seq := 0
			insert := func(at time.Time) {
				t.Helper()
				var err error
				seq++
				switch table {
				case "a2a_tasks":
					_, err = s.db.Exec(`INSERT INTO a2a_tasks(binding_id,task_id,state,version,task_json,created_ns,updated_ns) VALUES(?,?,?,1,'{}',?,?)`, "audit", fmt.Sprint(seq), string(a2a.TaskStateCompleted), at.UnixNano(), at.UnixNano())
				case "identity_audit":
					_, err = s.db.Exec(`INSERT INTO identity_audit(at,mode,authentication,method,route) VALUES(?,'observe','verified','GET','/test')`, at.UTC().Format(time.RFC3339Nano))
				case "events":
					insertEventAt(t, s, at)
				case "proxy_events":
					err = s.AppendProxyEvent(ProxyEvent{Server: "s", ToolName: "t", OK: true, Timestamp: at,
						Attribution: callcontext.Snapshot{Verified: true, PrincipalID: "session:s", SessionID: "s", AgentURN: "msg://agent/local/a"}, ClaimedSessionID: "unverified-claim"})
				case "ai_events":
					err = s.RecordAIAuditEvent(AIEvent{EventType: "chat", Operation: "chat", Success: true, Timestamp: at})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			// Insert recent first: caller-stamped histories need not be in age order.
			insert(now)
			insert(cutoff)
			insert(cutoff.Add(-time.Hour))
			insert(cutoff.Add(-2 * time.Hour))
			batch, err := s.DeleteEventHistoryBefore(context.Background(), table, cutoff, 1)
			if err != nil || batch.Removed != 1 || batch.AuditID == 0 {
				t.Fatalf("batch = %+v, %v", batch, err)
			}
			var name, stamp string
			var n int64
			if err := s.db.QueryRow(`SELECT table_name, cutoff, removed FROM retention_audit WHERE id=?`, batch.AuditID).Scan(&name, &stamp, &n); err != nil {
				t.Fatal(err)
			}
			if name != table || stamp != cutoff.Format(time.RFC3339Nano) || n != 1 {
				t.Fatalf("receipt: %s %s %d", name, stamp, n)
			}
			// A failed audit write must roll back deletion.
			_, err = s.db.Exec(`CREATE TRIGGER reject_retention_audit BEFORE INSERT ON retention_audit BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`)
			if err != nil {
				t.Fatal(err)
			}
			if batch, err := s.DeleteEventHistoryBefore(context.Background(), table, cutoff, 10); err == nil || batch.Removed != 0 {
				t.Fatalf("failed receipt: %+v, %v", batch, err)
			}
			if _, err := s.db.Exec(`DROP TRIGGER reject_retention_audit`); err != nil {
				t.Fatal(err)
			}
			batch, err = s.DeleteEventHistoryBefore(context.Background(), table, cutoff, 10)
			if err != nil || batch.Removed != 1 {
				t.Fatalf("rollback lost row: %+v, %v", batch, err)
			}
			batch, err = s.DeleteEventHistoryBefore(context.Background(), table, cutoff, 10)
			if err != nil || batch.Removed != 0 || batch.AuditID != 0 {
				t.Fatalf("recent/boundary removed: %+v, %v", batch, err)
			}
		})
	}
}

func TestEventHistoryAppendKeepsMoreThanFormerRing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	// Efficiently seed a full former ring, then exercise the real append paths.
	for _, query := range []string{
		`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<2000) INSERT INTO proxy_events(server,tool_name,duration_ms,ok,timestamp) SELECT 's','t',0,1,? FROM n`,
		`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<2000) INSERT INTO ai_events(event_type,operation,timestamp) SELECT 'chat','chat',? FROM n`,
	} {
		if _, err := s.db.Exec(query, now.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendProxyEvent(ProxyEvent{Server: "s", ToolName: "t", Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAIAuditEvent(AIEvent{EventType: "chat", Operation: "chat", Timestamp: now}); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountProxyEvents(ProxyEventFilter{})
	if err != nil || n != 2001 {
		t.Fatalf("proxy history: %d, %v", n, err)
	}
	summary, err := s.QueryAIUsageSummary(AIUsageFilter{})
	if err != nil || summary.Requests != 2001 {
		t.Fatalf("AI history: %+v, %v", summary, err)
	}
}

// Exercise the SDK states persisted by the adapter, composite keys, strict
// cutoff and last-update age. Unknown future states must fail safe.
func TestA2ATaskRetentionPreservesNonTerminalAndRecent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
	old := cutoff.Add(-time.Hour)
	states := []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected, a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired, a2a.TaskStateUnspecified, "future-state"}
	for _, state := range states {
		for _, binding := range []string{"old", "boundary", "recent"} {
			updated := old
			if binding == "boundary" {
				updated = cutoff
			}
			if binding == "recent" {
				updated = cutoff.Add(time.Hour)
			}
			_, err := s.db.Exec(`INSERT INTO a2a_tasks(binding_id,task_id,state,version,task_json,created_ns,updated_ns) VALUES(?,?,?,1,'{}',?,?)`, binding, string(state), string(state), old.UnixNano(), updated.UnixNano())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	batch, err := s.DeleteEventHistoryBefore(context.Background(), "a2a_tasks", cutoff, 1000)
	if err != nil || batch.Removed != 4 || batch.AuditID == 0 {
		t.Fatalf("batch: %+v, %v", batch, err)
	}
	for _, state := range states {
		for _, binding := range []string{"old", "boundary", "recent"} {
			_, err := s.GetA2ATask(binding, string(state))
			if binding == "old" && state.Terminal() {
				if !errors.Is(err, ErrA2ATaskNotFound) {
					t.Fatalf("expired %s/%s: %v", binding, state, err)
				}
			} else if err != nil {
				t.Fatalf("preserved %s/%s: %v", binding, state, err)
			}
		}
	}
}
