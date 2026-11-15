package eventstore_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 123456789, time.UTC)

func newStore(s *store.Stores) (*eventstore.Store, *storetest.Clock) {
	es := eventstore.New(s)
	clock := storetest.NewClock(t0)
	es.Now = clock.Now
	ids := storetest.NewIDs("evt_")
	es.NewEventID = ids.New
	es.NewInputID = storetest.NewIDs("ti_").New
	return es, clock
}

func exec(t *testing.T, h *store.Handle, q string, args ...any) {
	t.Helper()
	err := h.WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, tx.Dialect().Rebind(q), args...)
		return err
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func full() eventstore.Row {
	off := int64(0) // a real offset: zero must survive as 0, not NULL
	return eventstore.Row{
		Scope: "session", SessionID: "ses-1", Kind: "session.turn_output", PayloadJSON: `{"text":"hi"}`,
		EnvironmentID: "env", Authority: "auth", WorkerID: "w", Host: "h", ProjectID: "p", WorkstreamID: "ws",
		EpicID: "ep", SprintID: "sp", PlanID: "pl", PhaseID: "ph", TaskID: "t", RunID: "r", Attempt: 2,
		LaunchID: "l", AgentURN: "msg://agent/x/y", LogicalAgentID: "la", TeamRunID: "tr", Cell: "c",
		Category: "cat", Severity: "info", Sensitivity: "private", PII: true, Visibility: "v", PayloadSchema: "ps",
		Sanitized: true, Redactions: 3, LabelsJSON: `{"k":"v"}`, ContentType: "application/json", Truncated: true,
		TraceID: "tid", SpanID: "sid", CausationID: "cid", CorrelationID: "corr", ThreadID: "th", IdempotencyKey: "ik", Depth: 4,
		Traceparent: "00-a-b-01", InReplyTo: "irt",
		ActorKind: "agent", ActorURN: "msg://agent/x/z", ActorVerified: "jwt", OnBehalfOf: "obo", ActorRole: "role",
		OnBehalfOfJSON: `[{"urn":"u"}]`, SchemaVersion: "1.1", ProducerApp: "app", Subject: "subj", TurnID: "turn", ParentID: "par",
		RawOffset: &off, ProcessPID: 42, ProcessProvider: "claude", ProcessRuntime: "rt", ProviderSessionID: "psid",
		SourceChannel: "stdout", SourceConfidence: "high", ProducerSequence: 1 << 63, ProducerGeneration: 7, Summary: "sum",
	}
}

func TestInsertRoundTripsEveryColumn(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		in := full()
		in.At = t0.Add(-time.Minute)
		stored, err := es.Insert(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Seq == 0 || stored.EventID == "" || !stored.RecordedAt.Equal(t0) || !stored.At.Equal(in.At) {
			t.Fatalf("stored = seq %d id %q recorded %v at %v", stored.Seq, stored.EventID, stored.RecordedAt, stored.At)
		}
		got, ok, err := es.ByEventID(ctx, stored.EventID)
		if err != nil || !ok {
			t.Fatalf("ByEventID: %v %v", ok, err)
		}
		if !reflect.DeepEqual(got, stored) {
			t.Fatalf("read back differs:\n got  %+v\n want %+v", got, stored)
		}
		// A zero row: every optional column stays NULL.
		bare, err := es.Insert(ctx, eventstore.Row{Scope: "global", Kind: "k"})
		if err != nil {
			t.Fatal(err)
		}
		var nonNull int
		r := s.B().Reader()
		if err := r.QueryRowContext(ctx, r.Dialect().Rebind(`SELECT COUNT(*) FROM events WHERE id = ? AND (session_id IS NOT NULL OR payload_json IS NOT NULL
 OR scope_attempt IS NOT NULL OR pii IS NOT NULL OR raw_offset IS NOT NULL OR producer_sequence IS NOT NULL OR origin_at IS NOT NULL OR epoch IS NOT NULL)`), bare.Seq).Scan(&nonNull); err != nil {
			t.Fatal(err)
		}
		if nonNull != 0 {
			t.Fatal("empty fields were not stored as NULL")
		}
	})
}

func TestInsertRules(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		first, err := es.Insert(ctx, eventstore.Row{Scope: "session", Kind: "k", EventID: "evt_fixed"})
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"session.request_status", "session.work_state_changed"} {
			if _, err := es.Insert(ctx, eventstore.Row{Scope: "session", Kind: kind}); !errors.Is(err, eventstore.ErrBookkeepingKind) {
				t.Fatalf("%s as a plain event: %v", kind, err)
			}
		}
		var again eventstore.Row
		var existing bool
		if err := s.B().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
			again, existing, err = es.InsertTx(ctx, tx, eventstore.Row{Scope: "session", Kind: "other", EventID: "evt_fixed"}, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !existing || again.Seq != first.Seq || again.Kind != "k" {
			t.Fatalf("repeat of an event id: existing=%v seq %d/%d kind %q", existing, again.Seq, first.Seq, again.Kind)
		}
		if n, _ := es.Count(ctx); n != 1 {
			t.Fatalf("count %d, want 1", n)
		}
		future := t0.Add(6 * time.Minute)
		skewed, err := es.Insert(ctx, eventstore.Row{Scope: "session", Kind: "k", At: future})
		if err != nil {
			t.Fatal(err)
		}
		if !skewed.At.Equal(t0) || !skewed.OriginAt.Equal(future) {
			t.Fatalf("future at: at %v origin %v", skewed.At, skewed.OriginAt)
		}
		if _, err := es.Insert(ctx, eventstore.Row{Scope: "session", Kind: "k", Sensitivity: "secret"}); err == nil {
			t.Fatal("a secret event was stored")
		}
		if _, err := es.Insert(ctx, eventstore.Row{Scope: "session", Kind: "k", Sensitivity: "weird"}); err == nil {
			t.Fatal("an unknown sensitivity was stored")
		}
		if _, err := es.Insert(ctx, eventstore.Row{Kind: "k"}); !errors.Is(err, eventstore.ErrScopeRequired) {
			t.Fatalf("no scope: %v", err)
		}
		// Admission (class A tables) sets the retention class; a caller's value is ignored.
		exec(t, s.A(), `INSERT INTO event_retention_policies (id, policy_json, created_at) VALUES (?, ?, ?)`, "pol-1", `{}`, "2026-11-01T00:00:00Z")
		exec(t, s.A(), `INSERT INTO event_retention_admission (kind, retention_class, policy_id) VALUES (?, ?, ?)`, "debug.kind", "R-debug", "pol-1")
		admitted, err := es.Insert(ctx, eventstore.Row{Scope: "session", Kind: "debug.kind", RetentionClass: "R-audit"})
		if err != nil {
			t.Fatal(err)
		}
		if admitted.RetentionClass != "R-debug" || admitted.RetentionPolicyID != "pol-1" {
			t.Fatalf("admission: %q %q", admitted.RetentionClass, admitted.RetentionPolicyID)
		}
		plain, _ := es.Insert(ctx, eventstore.Row{Scope: "session", Kind: "k2", RetentionClass: "R-audit"})
		if plain.RetentionClass != "" {
			t.Fatalf("an unadmitted kind kept the caller's class %q", plain.RetentionClass)
		}
	})
}

func seed(t *testing.T, es *eventstore.Store, rows ...eventstore.Row) []eventstore.Row {
	t.Helper()
	var out []eventstore.Row
	for _, r := range rows {
		got, err := es.Insert(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got)
	}
	return out
}

func seqs(rows []eventstore.Row) []int64 {
	out := []int64{}
	for _, r := range rows {
		out = append(out, r.Seq)
	}
	return out
}

func TestReads(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		if lo, hi, err := es.StreamBounds(ctx); err != nil || lo != 0 || hi != 0 {
			t.Fatalf("empty bounds %d %d %v", lo, hi, err)
		}
		rs := seed(t, es,
			eventstore.Row{Scope: "session", SessionID: "a", Kind: "x"},
			eventstore.Row{Scope: "global", Kind: "y"},
			eventstore.Row{Scope: "session", SessionID: "a", Kind: "y"},
			eventstore.Row{Scope: "session", SessionID: "b", Kind: "x"},
		)
		a1, g, a2, b := rs[0].Seq, rs[1].Seq, rs[2].Seq, rs[3].Seq
		check := func(name string, got []eventstore.Row, err error, want ...int64) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if w := append([]int64{}, want...); !reflect.DeepEqual(seqs(got), w) {
				t.Fatalf("%s = %v, want %v", name, seqs(got), w)
			}
		}
		got, err := es.ListBySession(ctx, "a", 0, 0)
		check("ListBySession", got, err, a2, a1)
		got, err = es.ListBySession(ctx, "a", 0, a2)
		check("ListBySession cursor", got, err, a1)
		got, err = es.Since(ctx, a1)
		check("Since", got, err, g, a2, b)
		got, err = es.Page(ctx, eventstore.Filter{Scopes: []string{"session"}, Kinds: []string{"x"}}, 0, b, 0)
		check("Page", got, err, a1, b)
		got, err = es.Page(ctx, eventstore.Filter{SessionID: "a"}, a1, b, 0)
		check("Page window", got, err, a2)
		got, err = es.Query(ctx, eventstore.Filter{Kinds: []string{"y"}})
		check("Query kinds", got, err, a2, g)
		got, err = es.Query(ctx, eventstore.Filter{SinceSeq: a1, Cursor: b, Scopes: []string{"global", "session"}})
		check("Query since/cursor", got, err, a2, g)
		got, err = es.Recent(ctx, 2)
		check("Recent", got, err, b, a2)
		if n, _ := es.Count(ctx); n != 4 {
			t.Fatalf("Count %d", n)
		}
		if m, _ := es.MaxSeq(ctx); m != b {
			t.Fatalf("MaxSeq %d", m)
		}
		if lo, hi, _ := es.StreamBounds(ctx); lo != a1 || hi != b {
			t.Fatalf("bounds %d %d", lo, hi)
		}
		if _, ok, err := es.ByEventID(ctx, "evt_missing"); ok || err != nil {
			t.Fatalf("missing event id: %v %v", ok, err)
		}
	})
}

func TestHasPublishedProviderResult(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		seed(t, es,
			eventstore.Row{Scope: "session", SessionID: "s", Kind: eventstore.TurnOutputKind, PayloadJSON: `{"provider_result_id":"res_1"}`},
			eventstore.Row{Scope: "session", SessionID: "s", Kind: eventstore.TurnOutputKind, PayloadJSON: `not json res_2`},
			eventstore.Row{Scope: "session", SessionID: "s", Kind: "other", PayloadJSON: `{"provider_result_id":"res_3"}`},
			eventstore.Row{Scope: "session", SessionID: "s", Kind: eventstore.TurnOutputKind, PayloadJSON: `{"provider_result_id":"res_a_b"}`},
		)
		for id, want := range map[string]bool{"res_1": true, "res_2": false, "res_3": false, "res_a_b": true, "res_a%": false, "res__": false, "": false} {
			got, err := es.HasPublishedProviderResult(ctx, "s", id)
			if err != nil || got != want {
				t.Errorf("HasPublishedProviderResult(%q) = %v, %v; want %v", id, got, err, want)
			}
		}
	})
}

func TestOutbox(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		var pending []int64
		if err := s.B().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
			for i := 0; i < 3; i++ {
				r, _, err := es.InsertTx(ctx, tx, eventstore.Row{Scope: "global", Kind: "k"}, true)
				if err != nil {
					return err
				}
				pending = append(pending, r.Seq)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		seed(t, es, eventstore.Row{Scope: "global", Kind: "settled"})
		got, err := es.Pending(ctx, 0, 0)
		if err != nil || !reflect.DeepEqual(seqs(got), pending) || !got[0].Pending {
			t.Fatalf("Pending = %v %v", seqs(got), err)
		}
		if got, _ = es.Pending(ctx, pending[2], 0); !reflect.DeepEqual(seqs(got), pending[:2]) {
			t.Fatalf("Pending before = %v", seqs(got))
		}
		if err := es.MarkPublished(ctx, pending[:2]); err != nil {
			t.Fatal(err)
		}
		if got, _ = es.Pending(ctx, 0, 0); !reflect.DeepEqual(seqs(got), pending[2:]) {
			t.Fatalf("Pending after publish = %v", seqs(got))
		}
	})
}

func TestIngest(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		ev := eventstore.Row{Scope: "global", Kind: "app.thing", EventID: "evt_app_1"}
		stored, outcome, err := es.IngestEvent(ctx, ev, "sha256:aa", "nanite", t0, nil)
		if err != nil || outcome != eventstore.IngestStored || !stored.Pending {
			t.Fatalf("first ingest: %v %v pending=%v", outcome, err, stored.Pending)
		}
		if _, outcome, _ = es.IngestEvent(ctx, ev, "sha256:aa", "nanite", t0, nil); outcome != eventstore.IngestDuplicate {
			t.Fatalf("same digest: %v", outcome)
		}
		if _, outcome, _ = es.IngestEvent(ctx, ev, "sha256:bb", "nanite", t0, nil); outcome != eventstore.IngestConflict {
			t.Fatalf("other digest: %v", outcome)
		}
		seed(t, es, eventstore.Row{Scope: "global", Kind: "local", EventID: "evt_local"})
		if _, outcome, _ = es.IngestEvent(ctx, eventstore.Row{Scope: "global", Kind: "x", EventID: "evt_local"}, "d", "app", t0, nil); outcome != eventstore.IngestUnverifiable {
			t.Fatalf("local event id: %v", outcome)
		}
		if _, _, err := es.IngestEvent(ctx, eventstore.Row{Scope: "global", Kind: "x"}, "d", "app", t0, nil); err == nil {
			t.Fatal("an ingest without an event id was accepted")
		}
		if d, ok, _ := es.IngestDigest(ctx, "evt_app_1"); !ok || d != "sha256:aa" {
			t.Fatalf("IngestDigest %q %v", d, ok)
		}
		if n, _ := es.Count(ctx); n != 2 {
			t.Fatalf("count %d, want 2", n)
		}
		// Concurrent retries of one event id: one stores it, the rest report Duplicate. The insert
		// hook holds each racer until every one has reached it (or 300ms pass), which forces them all
		// past the existence check together on Postgres; with the id claimed first, only one reaches
		// it and the others wait on its digest row.
		const racers = 4
		var arrived atomic.Int32
		var open sync.Once
		gate := make(chan struct{})
		hold := func(ctx context.Context, tx store.Tx, r eventstore.Row) (eventstore.Row, error) {
			if arrived.Add(1) >= racers {
				open.Do(func() { close(gate) })
			}
			select {
			case <-gate:
			case <-time.After(300 * time.Millisecond):
			}
			stored, _, err := es.InsertTx(ctx, tx, r, true)
			return stored, err
		}
		outcomes := make(chan eventstore.IngestOutcome, racers)
		errs := make(chan error, racers)
		var wg sync.WaitGroup
		for range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, o, err := es.IngestEvent(ctx, eventstore.Row{Scope: "global", Kind: "app.thing", EventID: "evt_race"}, "sha256:cc", "nanite", t0, hold)
				outcomes <- o
				errs <- err
			}()
		}
		wg.Wait()
		close(outcomes)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent ingest: %v", err)
			}
		}
		count := map[eventstore.IngestOutcome]int{}
		for o := range outcomes {
			count[o]++
		}
		if count[eventstore.IngestStored] != 1 || count[eventstore.IngestDuplicate] != racers-1 {
			t.Fatalf("concurrent outcomes %v", count)
		}
	})
}

func TestRetentionSweeps(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		old := seed(t, es,
			eventstore.Row{Scope: "global", Kind: "k", At: t0.Add(-48 * time.Hour)},
			eventstore.Row{Scope: "global", Kind: "k", At: t0.Add(-47 * time.Hour)},
		)
		seed(t, es, eventstore.Row{Scope: "global", Kind: "k", At: t0})
		b, err := es.DeleteHistoryBefore(ctx, "events", t0.Add(-time.Hour), 1)
		if err != nil || b.Removed != 1 || b.AuditID == 0 {
			t.Fatalf("batch %+v %v", b, err)
		}
		if _, ok, _ := es.ByEventID(ctx, old[0].EventID); ok {
			t.Fatal("oldest event survived")
		}
		if b, _ = es.DeleteHistoryBefore(ctx, "events", t0.Add(-time.Hour), 10); b.Removed != 1 {
			t.Fatalf("second batch removed %d", b.Removed)
		}
		if b, _ = es.DeleteHistoryBefore(ctx, "events", t0.Add(-time.Hour), 10); b.Removed != 0 || b.AuditID != 0 {
			t.Fatalf("empty batch %+v", b)
		}
		if _, err := es.DeleteHistoryBefore(ctx, "ai_events", t0, 10); err == nil {
			t.Fatal("ai_events (dropped by D3) was accepted")
		}
		if _, err := es.DeleteHistoryBefore(ctx, "events", t0, 0); err == nil {
			t.Fatal("limit 0 was accepted")
		}
		// a2a_tasks is class C: the sweep runs as the allowlisted joint and still writes the B receipt.
		exec(t, s.C(), `INSERT INTO a2a_tasks (binding_id, task_id, state, version, task_json, created_ns, updated_ns) VALUES (?, ?, ?, 1, '{}', ?, ?)`,
			"b1", "t1", "TASK_STATE_COMPLETED", int64(1), t0.Add(-48*time.Hour).UnixNano())
		exec(t, s.C(), `INSERT INTO a2a_tasks (binding_id, task_id, state, version, task_json, created_ns, updated_ns) VALUES (?, ?, ?, 1, '{}', ?, ?)`,
			"b1", "t2", "TASK_STATE_WORKING", int64(1), t0.Add(-48*time.Hour).UnixNano())
		b, err = es.DeleteHistoryBefore(ctx, "a2a_tasks", t0.Add(-time.Hour), 10)
		if err != nil || b.Removed != 1 || b.AuditID == 0 {
			t.Fatalf("a2a_tasks batch %+v %v", b, err)
		}
		var audits int
		r := s.B().Reader()
		if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM retention_audit`).Scan(&audits); err != nil || audits != 3 {
			t.Fatalf("retention_audit rows %d %v", audits, err)
		}
	})
}

func TestTombstonePayloads(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		exec(t, s.A(), `INSERT INTO event_retention_policies (id, policy_json, created_at) VALUES (?, ?, ?)`, "pol-1", `{"v":1}`, "2026-11-01T00:00:00Z")
		exec(t, s.A(), `INSERT INTO event_retention_admission (kind, retention_class, policy_id) VALUES (?, ?, ?)`, "k", "R-activity", "pol-1")
		rs := seed(t, es,
			eventstore.Row{Scope: "global", Kind: "k", PayloadJSON: `{"keep":true}`},
			eventstore.Row{Scope: "global", Kind: "k", PayloadJSON: `{"drop":true}`, PII: true},
			eventstore.Row{Scope: "global", Kind: "unadmitted", PayloadJSON: `{"x":1}`},
		)
		var seen []eventstore.TombstoneCandidate
		batch, err := es.TombstonePayloads(ctx, 0, 10, func(c eventstore.TombstoneCandidate) (eventstore.TombstoneDecision, error) {
			seen = append(seen, c)
			return eventstore.TombstoneDecision{Tombstone: c.PII == 1, Digest: "withheld"}, nil
		})
		if err != nil || batch.Scanned != 2 || batch.Tombstoned != 1 || batch.NextCursor != rs[1].Seq {
			t.Fatalf("batch %+v %v", batch, err)
		}
		if len(seen) != 2 || seen[0].PolicyJSON != `{"v":1}` || seen[0].RetentionClass != "R-activity" || !seen[0].RecordedAt.Equal(t0) {
			t.Fatalf("candidates %+v", seen)
		}
		got, _, _ := es.ByEventID(ctx, rs[1].EventID)
		if got.PayloadJSON != "" || got.PayloadDigest != "withheld" {
			t.Fatalf("tombstoned row: payload %q digest %q", got.PayloadJSON, got.PayloadDigest)
		}
		boom := errors.New("boom")
		if _, err := es.TombstonePayloads(ctx, 0, 10, func(eventstore.TombstoneCandidate) (eventstore.TombstoneDecision, error) {
			return eventstore.TombstoneDecision{Tombstone: true, Digest: "withheld"}, boom
		}); !errors.Is(err, boom) {
			t.Fatalf("decide error: %v", err)
		}
		if got, _, _ := es.ByEventID(ctx, rs[0].EventID); got.PayloadJSON == "" {
			t.Fatal("a failed pass wrote a tombstone")
		}
		// A candidate whose policy row is missing fails the pass before decide, with nothing written.
		// (Admission only stamps stored policies, so the orphan reference is written directly, as a
		// policy row deleted after admission would leave it.)
		orphan := seed(t, es, eventstore.Row{Scope: "global", Kind: "orphan", PayloadJSON: `{"o":1}`})
		exec(t, s.B(), `UPDATE events SET retention_class = ?, retention_policy_id = ? WHERE id = ?`, "R-activity", "pol-missing", orphan[0].Seq)
		called := false
		if _, err := es.TombstonePayloads(ctx, 0, 10, func(eventstore.TombstoneCandidate) (eventstore.TombstoneDecision, error) {
			called = true
			return eventstore.TombstoneDecision{Tombstone: true, Digest: "withheld"}, nil
		}); err == nil || called {
			t.Fatalf("missing policy: err %v, decide called %v", err, called)
		}
		for _, r := range []eventstore.Row{rs[0], orphan[0]} {
			if got, _, _ := es.ByEventID(ctx, r.EventID); got.PayloadJSON == "" {
				t.Fatalf("missing-policy pass tombstoned event %d", r.Seq)
			}
		}
	})
}

func TestProxyEvents(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, clock := newStore(s)
		for _, ev := range []eventstore.ProxyEvent{
			{SessionID: "s1", Server: "torque", ToolName: "Torque_Task_Get", DurationMs: 5, OK: true},
			{Server: "torque", ToolName: "torque_task_list", DurationMs: 9, Error: "boom", Timestamp: t0.Add(time.Second)},
			{SessionID: "s2", Server: "tesseract", ToolName: "tesseract_recall", OK: true, AttributionJSON: `{"verified":true}`, TelemetryJSON: `{"bytes":3}`},
		} {
			if err := es.AppendProxyEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
		clock.Advance(time.Minute)
		all, err := es.QueryProxyEvents(ctx, eventstore.ProxyEventFilter{})
		if err != nil || len(all) != 3 {
			t.Fatalf("all %d %v", len(all), err)
		}
		if !all[0].Timestamp.Equal(t0) || all[0].AttributionJSON != "{}" || all[2].TelemetryJSON != `{"bytes":3}` || !all[0].OK || all[1].OK {
			t.Fatalf("rows %+v", all)
		}
		for name, c := range map[string]struct {
			f    eventstore.ProxyEventFilter
			want int
		}{
			"server":         {eventstore.ProxyEventFilter{ServerID: "torque"}, 2},
			"tool prefix ci": {eventstore.ProxyEventFilter{ToolName: "torque_task"}, 2},
			"session":        {eventstore.ProxyEventFilter{SessionID: "s2"}, 1},
			"errors":         {eventstore.ProxyEventFilter{ErrorsOnly: true}, 1},
			"since":          {eventstore.ProxyEventFilter{Since: t0}, 1},
		} {
			got, err := es.QueryProxyEvents(ctx, c.f)
			n, cerr := es.CountProxyEvents(ctx, c.f)
			if err != nil || cerr != nil || len(got) != c.want || n != c.want {
				t.Errorf("%s: query %d count %d (%v %v), want %d", name, len(got), n, err, cerr, c.want)
			}
		}
	})
}

func TestToolCallMetrics(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		for i, p := range []string{
			`{"tool_name":"t1","server":"torque","ok":true,"duration_ms":5,"gateway_ms":1,"forward_ms":4,"args_bytes":10,"result_bytes":100,"discovery_mode":"flat"}`,
			`{"tool_name":"t1","server":"torque","ok":true,"duration_ms":150}`,
			`{"tool_name":"t1","server":"torque","ok":false,"error_class":"timeout","duration_ms":9000}`,
			`{"tool_name":"t2","server":"tesseract","ok":1,"duration_ms":20}`,
			`not json`,
		} {
			seed(t, es, eventstore.Row{Scope: "global", Kind: "tool_call_end", PayloadJSON: p, At: t0.Add(time.Duration(i) * time.Second)})
		}
		bounds := []int{10, 100, 1000}
		groups, err := es.QueryToolCallMetrics(ctx, eventstore.MetricsQuery{BoundsMs: bounds})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]eventstore.MetricGroup{}
		for _, g := range groups {
			got[g.Tool+"/"+g.Outcome] = g
		}
		ok := got["t1/ok"]
		if len(groups) != 3 || ok.Calls != 2 || ok.ArgsBytes != 10 || ok.ResultBytes != 100 || ok.MetadataSamples != 1 {
			t.Fatalf("groups %+v", groups)
		}
		if ok.Duration.SumMs != 155 || !reflect.DeepEqual(ok.Duration.Counts, []int64{1, 1, 2}) || !reflect.DeepEqual(ok.Gateway.Counts, []int64{1, 1, 1}) {
			t.Fatalf("t1/ok histograms %+v %+v", ok.Duration, ok.Gateway)
		}
		if got["t1/timeout"].Calls != 1 || got["t2/ok"].Calls != 1 {
			t.Fatalf("outcomes %+v", groups)
		}
		if groups[0].Tool != "t1" || groups[0].Outcome != "ok" {
			t.Fatalf("order: first %+v", groups[0])
		}
		// Payloads Postgres's jsonb rejects (invalid, or valid JSON holding a \u0000 escape) must not
		// fail a filtered query there. SQLite counts the \u0000 one (json_valid accepts it); Postgres
		// skips it, which is why it is under another tool name.
		seed(t, es,
			eventstore.Row{Scope: "global", Kind: "tool_call_end", PayloadJSON: `{"tool_name":"t1"`, At: t0.Add(time.Minute)},
			eventstore.Row{Scope: "global", Kind: "tool_call_end", PayloadJSON: `{"tool_name":"t9","note":"a\u0000b","ok":true}`, At: t0.Add(time.Minute)},
		)
		if filtered, err := es.QueryToolCallMetrics(ctx, eventstore.MetricsQuery{Tool: "t1", BoundsMs: bounds}); err != nil || len(filtered) != 2 {
			t.Fatalf("filtered with bad payloads: %+v %v", filtered, err)
		}
		if _, err := es.QueryToolCallMetrics(ctx, eventstore.MetricsQuery{Tool: "t9", BoundsMs: bounds}); err != nil {
			t.Fatalf("filter on the \\u0000 payload: %v", err)
		}
		byMode, err := es.QueryToolCallMetrics(ctx, eventstore.MetricsQuery{ByMode: true, Tool: "t1", Since: t0, Until: t0.Add(2 * time.Second), BoundsMs: bounds})
		if err != nil || len(byMode) != 2 {
			t.Fatalf("by mode %+v %v", byMode, err)
		}
		modes := fmt.Sprint(byMode[0].Mode, ",", byMode[1].Mode)
		if !strings.Contains(modes, "flat") {
			t.Fatalf("modes %s", modes)
		}
	})
}

func TestTurnInputs(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		if _, err := es.RecordTurnInput(ctx, eventstore.TurnInput{SessionID: "s"}); err == nil {
			t.Fatal("an input without a source was recorded")
		}
		a, err := es.RecordTurnInput(ctx, eventstore.TurnInput{SessionID: "s", Source: eventstore.InputChatSend, SenderClass: eventstore.ClassOperator, Basis: eventstore.BasisPrincipal})
		if err != nil || a.Seq != 1 || a.Queued || a.State != eventstore.InputSubmitting {
			t.Fatalf("first input %+v %v", a, err)
		}
		if aud, _ := es.BindTurnInput(ctx, "s", "turn-0"); aud.Basis != "submission_unsettled" || aud.Audience != eventstore.ClassUnknown {
			t.Fatalf("unsettled bind %+v", aud)
		}
		if err := es.SettleTurnInput(ctx, a.ID, true); err != nil {
			t.Fatal(err)
		}
		b, _ := es.RecordTurnInput(ctx, eventstore.TurnInput{SessionID: "s", Source: eventstore.InputMailWake})
		if b.Seq != 2 || b.SenderClass != eventstore.ClassUnknown || b.Basis != eventstore.BasisNone {
			t.Fatalf("second input %+v", b)
		}
		// turn-0 bound a; it is not rebound.
		aud, err := es.BindTurnInput(ctx, "s", "turn-0")
		if err != nil || aud.Basis != "submission_unsettled" || len(aud.InputIDs) != 1 || aud.InputIDs[0] != a.ID || aud.Sources != nil {
			t.Fatalf("stored audience %+v %v", aud, err)
		}
		// b is now the oldest unbound live input; it is still submitting.
		if err := es.SettleTurnInput(ctx, b.ID, true); err != nil {
			t.Fatal(err)
		}
		aud, _ = es.BindTurnInput(ctx, "s", "turn-1")
		if aud.Audience != eventstore.ClassUnknown || aud.Basis != eventstore.BasisNone || !reflect.DeepEqual(aud.Sources, []string{eventstore.InputMailWake}) {
			t.Fatalf("unknown sender bind %+v", aud)
		}
		c, _ := es.RecordTurnInput(ctx, eventstore.TurnInput{SessionID: "s", Source: eventstore.InputSessionTurn, SenderClass: eventstore.ClassAgent, Basis: eventstore.BasisStampedSender})
		_ = es.SettleTurnInput(ctx, c.ID, true)
		aud, _ = es.BindTurnInput(ctx, "s", "turn-2")
		if aud.Audience != eventstore.ClassAgent || aud.Basis != eventstore.BasisStampedSender || !reflect.DeepEqual(aud.Classes, []string{eventstore.ClassAgent}) {
			t.Fatalf("proven bind %+v", aud)
		}
		if aud, _ = es.BindTurnInput(ctx, "s", "turn-3"); aud.Basis != "no_input" {
			t.Fatalf("nothing waiting %+v", aud)
		}
		l, _ := es.RecordTurnInput(ctx, eventstore.TurnInput{SessionID: "s2", Source: eventstore.InputSystem, RefID: "launch_prompt"})
		if err := es.SettleLaunchPromptInput(ctx, "s2", false); err != nil {
			t.Fatal(err)
		}
		var state string
		r := s.B().Reader()
		_ = r.QueryRowContext(ctx, r.Dialect().Rebind(`SELECT state FROM turn_inputs WHERE input_id = ?`), l.ID).Scan(&state)
		if state != eventstore.InputFailed {
			t.Fatalf("launch prompt state %q", state)
		}
	})
}

// TestWritesStayInClassB pins the S2a rule for this store: a class B transaction refuses a write to
// a class C table, so InsertTx inside the wrong handle fails loudly.
func TestWritesStayInClassB(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		es, _ := newStore(s)
		err := s.C().WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, _, err := es.InsertTx(ctx, tx, eventstore.Row{Scope: "global", Kind: "k"}, false)
			return err
		})
		if !errors.Is(err, store.ErrCrossClassWrite) {
			t.Fatalf("events insert in a class C transaction: %v", err)
		}
	})
}

// TestEventIDsCommitInOrder holds a transaction open after its insert while a second insert runs: a
// reader must not see the second (higher id) before the first commits, or a cursor past it would
// skip the first forever. On Postgres the identity alone allows that; LockEventSeq prevents it.
func TestEventIDsCommitInOrder(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		es, _ := newStore(s)
		inserted, release := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- s.B().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
				if _, _, err := es.InsertTx(ctx, tx, eventstore.Row{Scope: "global", Kind: "first"}, false); err != nil {
					return err
				}
				close(inserted)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
				return nil
			})
		}()
		<-inserted
		second := make(chan error, 1)
		go func() {
			_, err := es.Insert(ctx, eventstore.Row{Scope: "global", Kind: "second"})
			second <- err
		}()
		time.Sleep(300 * time.Millisecond)
		if seen, err := es.Since(ctx, 0); err != nil || len(seen) != 0 {
			t.Errorf("while the first insert is open, a reader sees %d events (%v): a later id committed first", len(seen), err)
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		seen, err := es.Since(ctx, 0)
		if err != nil || len(seen) != 2 || seen[0].Kind != "first" || seen[1].Kind != "second" {
			t.Fatalf("after both commit: %+v %v", seen, err)
		}
	})
}
