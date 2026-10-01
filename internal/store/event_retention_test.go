package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestEventHistoryRetentionAuditAndRollback(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-90 * 24 * time.Hour)
	for _, table := range []string{"events", "proxy_events", "ai_events"} {
		t.Run(table, func(t *testing.T) {
			insert := func(at time.Time) {
				t.Helper()
				var err error
				switch table {
				case "events":
					insertEventAt(t, s, at)
				case "proxy_events":
					err = s.AppendProxyEvent(ProxyEvent{Server: "s", ToolName: "t", OK: true, Timestamp: at})
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
