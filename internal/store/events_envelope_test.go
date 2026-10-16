package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// rowByID reads every column of one events row by name.
func rowByID(t *testing.T, db *Store, id int64) map[string]any {
	t.Helper()
	rows, err := db.db.Query(`SELECT * FROM events WHERE id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		t.Fatalf("no events row %d", id)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for i, c := range cols {
		out[c] = vals[i]
	}
	return out
}

func fullEnvelope() events.Event {
	return events.Event{
		Scope: events.ScopeSession, SessionID: "s1", Kind: "session.state_changed", PayloadJSON: `{"to":"running"}`,
		LogicalAgentID: "agent-legacy-field",
		Envelope: events.Envelope{
			Dims: events.ScopeDims{EnvironmentID: "env-1", Authority: "hub", WorkerID: "w1", Host: "h1", ProjectID: "p1", WorkstreamID: "ws1",
				EpicID: "EP-1", SprintID: "SP-1", PlanID: "PL-1", PhaseID: "ph-1", TaskID: "CW-1", RunID: "run-1", Attempt: 2, LaunchID: "l1",
				AgentURN: "msg://agent/x", LogicalAgentID: "agent-1", TeamRunID: "tr-1"},
			Class: events.Class{Category: "state", Severity: "info", Sensitivity: "private", PII: true, Visibility: "environment",
				PayloadSchema: "session.state_changed@1", Sanitized: true, Redactions: 3, Labels: []string{"a", "b"}},
			Corr:        events.Corr{TraceID: "t", SpanID: "sp", CausationID: "c", CorrelationID: "co", ThreadID: "th", IdempotencyKey: "ik", Depth: 1},
			Actor:       events.Actor{Kind: "agent", URN: "msg://agent/x", Verified: "token", OnBehalfOf: "msg://user/me"},
			ContentType: "application/json", Truncated: true,
			RetentionClass: events.RetentionState, RetentionPolicyID: "pol-abc",
			// producers cannot set these:
			RecordedAt: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), Epoch: 99,
		},
	}
}

func TestPublishPersistsTheEnvelopeAndTheLogAssignsRecordedAt(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	before := time.Now().UTC().Add(-time.Second)
	if err := bus.Publish(ctx, fullEnvelope()); err != nil {
		t.Fatal(err)
	}
	got := collect(t, live, 1, 5*time.Second)[0]
	row := rowByID(t, db, got.Seq)
	want := map[string]string{
		"scope_environment_id": "env-1", "scope_authority": "hub", "scope_worker_id": "w1", "scope_host": "h1", "scope_project_id": "p1",
		"scope_workstream_id": "ws1", "scope_epic_id": "EP-1", "scope_sprint_id": "SP-1", "scope_plan_id": "PL-1", "scope_phase_id": "ph-1",
		"scope_task_id": "CW-1", "scope_run_id": "run-1", "scope_launch_id": "l1", "scope_agent_urn": "msg://agent/x",
		"scope_logical_agent_id": "agent-1", "scope_team_run_id": "tr-1",
		"category": "state", "severity": "info", "sensitivity": "private", "visibility": "environment", "payload_schema": "session.state_changed@1",
		"labels_json": `["a","b"]`, "content_type": "application/json",
		"trace_id": "t", "span_id": "sp", "causation_id": "c", "correlation_id": "co", "thread_id": "th", "idempotency_key": "ik",
		"actor_kind": "agent", "actor_urn": "msg://agent/x", "actor_verified": "token", "on_behalf_of": "msg://user/me",
		"retention_class": "R-state", "retention_policy_id": "pol-abc",
		"scope": events.ScopeSession, "kind": "session.state_changed", "session_id": "s1",
	}
	for col, w := range want {
		if g, ok := row[col].(string); !ok || g != w {
			t.Errorf("%s = %#v, want %q", col, row[col], w)
		}
	}
	for col, w := range map[string]int64{"scope_attempt": 2, "pii": 1, "sanitized": 1, "redactions": 3, "truncated": 1, "depth": 1} {
		if g, ok := row[col].(int64); !ok || g != w {
			t.Errorf("%s = %#v, want %d", col, row[col], w)
		}
	}
	if row["payload_digest"] != nil || row["epoch"] != nil || row["pending"].(int64) != 0 {
		t.Errorf("digest/epoch/pending: %#v %#v %#v", row["payload_digest"], row["epoch"], row["pending"])
	}
	// the log assigns recorded_at; the producer's value (2001) and epoch are ignored
	rec, _ := row["recorded_at"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, rec)
	if err != nil || parsed.Before(before) || parsed.Year() == 2001 {
		t.Fatalf("recorded_at %q is not the log's own stamp", rec)
	}
	if got.EventID == "" || row["event_id"] != got.EventID || got.Envelope.RecordedAt.IsZero() {
		t.Fatalf("fanned-out event %+v vs row id %v", got, row["event_id"])
	}
}

// Without an envelope every v1.1 column is NULL except recorded_at; the legacy
// LogicalAgentID field on Event is the one dimension persisted today.
func TestPlainPublishLeavesTheEnvelopeNullButPersistsLogicalAgentID(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx := context.Background()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s", LogicalAgentID: "agent-7", Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "k2"}); err != nil {
		t.Fatal(err)
	}
	got := collect(t, live, 2, 5*time.Second)
	a, b := rowByID(t, db, got[0].Seq), rowByID(t, db, got[1].Seq)
	if a["scope_logical_agent_id"] != "agent-7" || b["scope_logical_agent_id"] != nil {
		t.Fatalf("logical agent id: %v / %v", a["scope_logical_agent_id"], b["scope_logical_agent_id"])
	}
	for _, row := range []map[string]any{a, b} {
		for _, col := range envelopeColumns {
			if col == "scope_logical_agent_id" || col == "recorded_at" {
				continue
			}
			if row[col] != nil {
				t.Errorf("%s = %#v, want NULL", col, row[col])
			}
		}
		if row["recorded_at"] == nil {
			t.Error("recorded_at not assigned")
		}
	}
}

func TestPublishTxPersistsTheEnvelopeToo(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	ctx := context.Background()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := fullEnvelope()
	stored, err := bus.PublishTx(ctx, tx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	row := rowByID(t, db, stored.Seq)
	if row["scope_task_id"] != "CW-1" || row["actor_verified"] != "token" || row["retention_class"] != "R-state" || row["pending"].(int64) != 1 {
		t.Fatalf("outbox row lost its envelope: %v", row)
	}
	if stored.Envelope.RecordedAt.IsZero() || stored.Envelope.RecordedAt.Year() == 2001 {
		t.Fatalf("recorded_at %v", stored.Envelope.RecordedAt)
	}
}

func TestInvalidRetentionClassIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	e := events.Event{Scope: events.ScopeSession, Kind: "k", Envelope: events.Envelope{RetentionClass: "R-bogus"}}
	if err := bus.Publish(context.Background(), e); err == nil {
		t.Fatal("unknown retention class accepted")
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused event left %d row(s) (%v)", n, err)
	}
}

// The schema itself enforces what the retention lane relies on.
func TestEnvelopeSchemaConstraints(t *testing.T) {
	db := openEventsStore(t)
	insert := func(extra string, args ...any) error {
		_, err := db.db.Exec(`INSERT INTO events (scope, session_id, at, kind, payload_json`+extra, args...)
		return err
	}
	if err := insert(`) VALUES ('session','s','2026-01-01T00:00:00Z','k','{}')`); err != nil {
		t.Fatalf("a legacy-shaped insert failed: %v", err)
	}
	if err := insert(`, retention_class) VALUES ('session','s','2026-01-01T00:00:00Z','k','{}','R-state')`); err != nil {
		t.Fatalf("valid class refused: %v", err)
	}
	if err := insert(`, retention_class) VALUES ('session','s','2026-01-01T00:00:00Z','k','{}','R-debugg')`); err == nil {
		t.Fatal("bad retention class accepted")
	}
	hex := strings.Repeat("a1", 32)
	for name, tc := range map[string]struct {
		digest  string
		payload string
		ok      bool
	}{
		"sha256 with the payload removed":         {"'sha256:" + hex + "'", "NULL", true},
		"withheld with the payload removed":       {"'withheld'", "NULL", true},
		"digest while the payload is still there": {"'sha256:" + hex + "'", "'{}'", false},
		"uppercase hex":                           {"'sha256:" + strings.ToUpper(hex) + "'", "NULL", false},
		"non-hex tail":                            {"'sha256:" + strings.Repeat("zz", 32) + "'", "NULL", false},
		"short":                                   {"'sha256:abcd'", "NULL", false},
		"wrong prefix":                            {"'md5:" + hex + "000'", "NULL", false},
		"anything else":                           {"'redacted'", "NULL", false},
	} {
		err := insert(`, payload_digest) VALUES ('session','s','2026-01-01T00:00:00Z','k',` + tc.payload + `,` + tc.digest + `)`)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, tc.ok)
		}
	}
	// the partial indexes exist
	for _, idx := range []string{"idx_events_scope_project", "idx_events_scope_task", "idx_events_scope_logical_agent", "idx_events_retention_class"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=? AND sql LIKE '%WHERE%'`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("partial index %s missing (%v)", idx, err)
		}
	}
}
