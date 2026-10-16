package environmentstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mesh "github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func storedExample() events.Event {
	return events.Event{Seq: 42, EventID: "evt_0123456789abcdef0123456789abcdef", At: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		Scope: events.ScopeSession, SessionID: "session", Kind: "unknown.future.kind", PayloadJSON: `{"count":3}`}
}

func TestStoredProjectionPreservesProducerEnvelope(t *testing.T) {
	e := storedExample()
	recorded := e.At.Add(time.Second)
	offset := int64(0)
	e.Envelope = events.Envelope{
		SchemaVersion: "1", App: "producer", Subject: "urn:task:example", TurnID: "turn", ParentID: "parent", RawOffset: &offset,
		Process: events.Process{PID: 71, Provider: "provider", Runtime: "runtime", ProviderSessionID: "native-session"},
		Source:  events.Source{Channel: "native.events", Confidence: "derived"}, Sequence: 9007199254740993, Generation: 7,
		RecordedAt: recorded, ContentType: "application/json", Truncated: true,
		Dims:  events.ScopeDims{EnvironmentID: "producer-env", WorkerID: "worker", ProjectID: "project", TaskID: "task", RunID: "run", Attempt: 2, LogicalAgentID: "logical", TeamRunID: "team"},
		Corr:  events.Corr{TraceID: "trace", SpanID: "span", Traceparent: "traceparent", InReplyTo: "reply", CausationID: "cause", CorrelationID: "correlation", ThreadID: "thread", IdempotencyKey: "once", Depth: 3},
		Actor: events.Actor{URN: "msg://agent/example/author", Kind: "agent", Role: "workflow", Verified: "token", OnBehalfOfChain: []string{"msg://user/example/operator"}},
		Class: events.Class{Category: "activity", Severity: "notice", Sensitivity: "private", PII: true, Visibility: "operator", PayloadSchema: "example@1", Labels: []string{"label"}, Sanitized: true, Redactions: 2},
	}
	out, err := (StoredIDProjection{Cutover: 50}).MapStored("log-env", e)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != e.EventID || out.Cursor != "42" || out.SourceSequence != e.Envelope.Sequence || out.Generation != 7 || out.Time != e.At || out.RecordedAt == nil || *out.RecordedAt != recorded {
		t.Fatalf("identity, cursor or time changed: %+v", out)
	}
	if out.App != "producer" || out.Subject != "urn:task:example" || out.TurnID != "turn" || out.ParentID != "parent" || out.Source.Channel != "native.events" || out.Source.Confidence != 0.5 || !out.Truncated {
		t.Fatalf("producer metadata lost: %+v", out)
	}
	if out.Actor.Kind != mesh.ActorAgent || out.ActorVerified != "token" || out.ActorRole != "workflow" || len(out.OnBehalfOf) != 1 || out.OnBehalfOf[0] != "msg://user/example/operator" {
		t.Fatalf("actor changed: %+v", out)
	}
	if out.Scope == nil || out.Scope.EnvironmentID != "producer-env" || out.Scope.Attempt != 2 || out.Scope.TeamRunID != "team" || out.Scope.LogicalAgentID != "logical" {
		t.Fatalf("scope lost: %+v", out.Scope)
	}
	if out.Trace.Traceparent != "traceparent" || out.Trace.TraceID != "trace" || out.Trace.SpanID != "span" || out.InReplyTo != "reply" || out.CausationID != "cause" || out.CorrelationID != "correlation" || out.ThreadID != "thread" || out.IdempotencyKey != "once" || out.Depth != 3 {
		t.Fatalf("correlation lost: %+v", out)
	}
	if out.Class == nil || !out.Class.PII || !out.Class.Sanitized || out.Class.Redactions != 2 || out.Class.Sensitivity != "private" || out.Visibility != "operator" || out.PayloadSchema != "example@1" {
		t.Fatalf("class lost: %+v", out)
	}
	var process map[string]any
	if err := json.Unmarshal(out.Process, &process); err != nil || process["provider_session_id"] != "native-session" || process["pid"] != float64(71) {
		t.Fatalf("process lost: %s (%v)", out.Process, err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil || string(wire["id"]) != `"`+e.EventID+`"` || string(wire["source_sequence"]) != "9007199254740993" || string(wire["cursor"]) != `"42"` {
		t.Fatalf("opaque ID or independent coordinates corrupted: %s (%v)", data, err)
	}
	*out.RawOffset = 9
	out.Class.Labels[0] = "changed"
	out.OnBehalfOf[0] = "msg://user/example/changed"
	if offset != 0 || e.Envelope.Class.Labels[0] != "label" || e.Envelope.Actor.OnBehalfOfChain[0] != "msg://user/example/operator" {
		t.Fatal("projection aliases producer metadata")
	}
}

func TestStoredProjectionLegacyAndActorFallback(t *testing.T) {
	for _, actor := range []events.Actor{{}, {Kind: "device", URN: "msg://device/example/worker", Verified: "token", OnBehalfOfChain: []string{"msg://user/example/owner"}}, {Kind: "agent", URN: "msg://user/example/mismatch", Verified: "token"}} {
		e := storedExample()
		e.EventID = "environment:42"
		e.LogicalAgentID = "logical"
		e.Envelope.Actor = actor
		out, err := (StoredIDProjection{Cutover: 42}).MapStored("environment", e)
		if err != nil {
			t.Fatal(err)
		}
		if out.ID != "environment:42" || out.Actor.URN != "msg://service/tether/environment" || out.ActorVerified != "none" || out.ActorRole != "system" || len(out.OnBehalfOf) != 0 {
			t.Fatalf("fallback claims authorship: %+v", out)
		}
		if out.Source.Channel != "tether.events" || out.SourceSequence != 42 || out.Generation != 1 || out.Visibility != "local" || out.Scope.LogicalAgentID != "logical" {
			t.Fatalf("legacy projection: %+v", out)
		}
	}
}

func TestStoredProjectionNullIDsAndTombstones(t *testing.T) {
	e := storedExample()
	e.EventID = ""
	if _, err := (StoredIDProjection{Cutover: 42}).MapStored("env", e); !errors.Is(err, ErrBackfillInProgress) {
		t.Fatalf("legacy NULL synthesized: %v", err)
	}
	if _, err := (StoredIDProjection{Cutover: 41}).MapStored("env", e); !errors.Is(err, store.ErrEventIDsAfterCutover) {
		t.Fatalf("new NULL treated as legacy: %v", err)
	}
	e = storedExample()
	e.Envelope.PayloadDigest = "withheld"
	e.PayloadJSON = "null"
	out, err := (StoredIDProjection{}).MapStored("env", e)
	if err != nil || out.PayloadDigest != "withheld" || string(out.Payload) != "null" {
		t.Fatalf("tombstone lost: %+v %v", out, err)
	}
	e.PayloadJSON = `{"must_not_reappear":true}`
	if _, err := (StoredIDProjection{}).MapStored("env", e); err == nil {
		t.Fatal("tombstone with retained payload accepted")
	}
	e = storedExample()
	e.Envelope.Source.Channel = "native.events"
	if _, err := (StoredIDProjection{}).MapStored("env", e); err == nil {
		t.Fatal("log cursor invented as native producer sequence")
	}
}

type projectionWindows struct {
	windows []store.EnvironmentWindow
	reads   int
}

func (s *projectionWindows) EnvironmentEvents(context.Context, int64, int) (store.EnvironmentWindow, error) {
	w := s.windows[s.reads]
	s.reads++
	return w, nil
}

func (*projectionWindows) EnvironmentSnapshot(context.Context, string) (store.EnvironmentSnapshot, error) {
	return store.EnvironmentSnapshot{Sessions: []store.EnvironmentSessionRow{{ID: "selected"}}}, nil
}

func TestStoredStreamRefusesNullBeforeSessionFiltering(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cutover int64
		status  int
		reason  string
	}{
		{"legacy", 42, http.StatusServiceUnavailable, "backfill_in_progress"},
		{"new", 41, http.StatusInternalServerError, "event_id_invariant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := storedExample()
			e.EventID = ""
			db := &projectionWindows{windows: []store.EnvironmentWindow{{Events: []events.Event{e}, HighWater: 42, EarliestAvailable: 1}}}
			s := &Server{EnvironmentID: "env", Store: db, Bus: &closingBus{live: make(chan events.Event)}, StoredIDs: &StoredIDProjection{Cutover: tc.cutover}}
			w := httptest.NewRecorder()
			s.Stream(w, httptest.NewRequest(http.MethodGet, "/?after_seq=41", nil), "selected")
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.reason) || strings.Contains(w.Body.String(), "synchronized") {
				t.Fatalf("missing row skipped: %d %s", w.Code, w.Body)
			}
			if tc.status == http.StatusServiceUnavailable && (w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"snapshot_required":false`)) {
				t.Fatalf("retry would discard history: %v %s", w.Header(), w.Body)
			}
		})
	}
}

func TestStoredStreamLaterBackfillGapDoesNotAdvanceCursor(t *testing.T) {
	e := storedExample()
	e.EventID = ""
	live := make(chan events.Event, 1)
	live <- e
	db := &projectionWindows{windows: []store.EnvironmentWindow{
		{HighWater: 41, EarliestAvailable: 1},
		{Events: []events.Event{e}, HighWater: 42, EarliestAvailable: 1},
	}}
	s := &Server{EnvironmentID: "env", Store: db, Bus: &closingBus{live: live}, StoredIDs: &StoredIDProjection{Cutover: 42}, PollInterval: time.Hour}
	w := httptest.NewRecorder()
	s.Stream(w, httptest.NewRequest(http.MethodGet, "/?after_seq=41", nil), "")
	if !strings.Contains(w.Body.String(), "id: 41\nevent: gap") || strings.Contains(w.Body.String(), "id: 42") || !strings.Contains(w.Body.String(), `"reason":"backfill_in_progress"`) || !strings.Contains(w.Body.String(), `"retry_after_seconds":1`) || !strings.Contains(w.Body.String(), `"snapshot_required":false`) {
		t.Fatalf("backfill gap advanced or dropped history: %s", w.Body)
	}
}

func TestStoredStreamRetryDeliversBackfilledIDFromSameCursor(t *testing.T) {
	e := storedExample()
	e.EventID = ""
	db := &projectionWindows{windows: []store.EnvironmentWindow{{Events: []events.Event{e}, HighWater: 42, EarliestAvailable: 1}}}
	s := &Server{EnvironmentID: "env", Store: db, Bus: &closingBus{live: make(chan events.Event)}, StoredIDs: &StoredIDProjection{Cutover: 42}}
	w := httptest.NewRecorder()
	s.Stream(w, httptest.NewRequest(http.MethodGet, "/?after_seq=41", nil), "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unfinished legacy row: %d", w.Code)
	}
	e.EventID = "env:42"
	db.windows = append(db.windows, store.EnvironmentWindow{Events: []events.Event{e}, HighWater: 42, EarliestAvailable: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retry := &cancelOnSyncWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	s.Stream(retry, httptest.NewRequest(http.MethodGet, "/?after_seq=41", nil).WithContext(ctx), "")
	if !strings.Contains(retry.Body.String(), `"id":"env:42"`) || !strings.Contains(retry.Body.String(), "id: 42\nevent: unknown.future.kind") || !strings.Contains(retry.Body.String(), `"cursor":"42"`) {
		t.Fatalf("retry skipped or synthesized the repaired event: %s", retry.Body)
	}
}

func TestStoredStreamInvariantIsNotHiddenByPendingLegacyRow(t *testing.T) {
	legacy := storedExample()
	legacy.EventID = ""
	invalid := legacy
	invalid.Seq = 43
	s := &Server{StoredIDs: &StoredIDProjection{Cutover: 42}}
	err := s.checkWindowIDs(store.EnvironmentWindow{Events: []events.Event{legacy, invalid}, HighWater: 43}, 41)
	if !errors.Is(err, store.ErrEventIDsAfterCutover) {
		t.Fatalf("bad rollback hidden behind unfinished legacy row: %v", err)
	}
}
