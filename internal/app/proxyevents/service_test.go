package proxyevents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

type testRows struct {
	rows                []store.ProxyEvent
	filter              store.ProxyEventFilter
	queryErr, appendErr error
}

func (s *testRows) QueryProxyEvents(f store.ProxyEventFilter) ([]store.ProxyEvent, error) {
	s.filter = f
	return s.rows, s.queryErr
}
func (s *testRows) AppendProxyEvent(e store.ProxyEvent) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	s.rows = append(s.rows, e)
	return nil
}

type testBus struct {
	events.Bus
	published []events.Event
	err       error
}

func (b *testBus) Publish(_ context.Context, e events.Event) error {
	b.published = append(b.published, e)
	return b.err
}

func TestQueryRecordsPreservesLegacyJSONAndNil(t *testing.T) {
	stamp := time.Date(2026, 10, 2, 1, 2, 3, 4, time.FixedZone("offset", 3600))
	row := store.ProxyEvent{ID: 12, SessionID: "own", Server: "server", ToolName: "tool", ArgsSchemaFP: "fp", DurationMs: 7, OK: false, Error: "failure", Timestamp: stamp, Attribution: callcontext.Snapshot{Verified: true, SessionID: "own", PrincipalID: "session:own"}, ClaimedSessionID: "other"}
	q := Query{ServerID: "server", ToolName: "tool", SessionID: "own", Limit: 50, Since: stamp, ErrorsOnly: true}
	for _, rows := range [][]store.ProxyEvent{nil, {}, {row}} {
		st := &testRows{rows: rows}
		got, err := QueryRecords(st, q)
		if err != nil {
			t.Fatal(err)
		}
		original, _ := json.Marshal(rows)
		actual, _ := json.Marshal(got)
		if string(actual) != string(original) {
			t.Fatalf("legacy JSON: got %s want %s", actual, original)
		}
		if !reflect.DeepEqual(st.filter, store.ProxyEventFilter{ServerID: q.ServerID, ToolName: q.ToolName, SessionID: q.SessionID, Limit: q.Limit, Since: q.Since, ErrorsOnly: q.ErrorsOnly}) {
			t.Fatalf("filter=%+v", st.filter)
		}
		if len(got) > 0 {
			dto := ToDTO(got[0])
			if dto.Timestamp != stamp.UTC().Format(time.RFC3339Nano) || dto.Attribution != row.Attribution || dto.ClaimedSessionID != row.ClaimedSessionID {
				t.Fatalf("HTTP DTO=%+v", dto)
			}
		}
	}
	st := &testRows{queryErr: errors.New("unavailable")}
	_, err := QueryRecords(st, q)
	if !errors.Is(err, st.queryErr) {
		t.Fatalf("query error identity=%v", err)
	}
}

func TestIngestValidationPrecedesTrustedResolution(t *testing.T) {
	st := &testRows{}
	bus := &testBus{}
	err := New(st, bus, nil).Ingest(context.Background(), ProxyEventIngestRequest{}, func() callcontext.Snapshot { t.Fatal("invalid body resolved caller"); return callcontext.Snapshot{} })
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "bad_request" || failure.Message != "tool_name is required" || len(st.rows) != 0 || len(bus.published) != 0 {
		t.Fatalf("validation=%v rows=%v bus=%v", err, st.rows, bus.published)
	}
}

func TestIngestTrustedAttributionAndPublishOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot callcontext.Snapshot
		session  string
	}{
		{"session", callcontext.Snapshot{Verified: true, PrincipalID: "session:own", SessionID: "own"}, "own"},
		{"operator", callcontext.Snapshot{PrincipalID: "operator", PrincipalKind: "operator"}, ""},
		{"anonymous", callcontext.Snapshot{}, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &testRows{}
			bus := &testBus{}
			looked := ""
			operation := New(st, bus, func(id string) error { looked = id; return nil })
			before := time.Now().UTC()
			err := operation.Ingest(context.Background(), ProxyEventIngestRequest{SessionID: "other", ToolName: "tool", Error: strings.Repeat("€", 5000), Timestamp: "2001-01-01T00:00:00Z", Publish: true, Attribution: callcontext.Snapshot{SessionID: "forged", Verified: true}}, func() callcontext.Snapshot { return tc.snapshot })
			if err != nil || len(st.rows) != 1 || len(bus.published) != 1 {
				t.Fatalf("ingest=%v rows=%v published=%v", err, st.rows, bus.published)
			}
			row := st.rows[0]
			if row.Attribution != tc.snapshot || row.SessionID != tc.session || row.ClaimedSessionID != "other" || row.Timestamp.Before(before) || row.Timestamp.After(time.Now()) || len(row.Error) > MaxProxyEventErrorBytes || looked != tc.session {
				t.Fatalf("row=%+v lookup=%q", row, looked)
			}
			var call events.ToolCallEvent
			if err := json.Unmarshal([]byte(bus.published[0].PayloadJSON), &call); err != nil {
				t.Fatal(err)
			}
			if call.Attribution != row.Attribution || call.SessionID != row.SessionID || call.Error != row.Error || call.ClaimedSessionID != row.ClaimedSessionID {
				t.Fatalf("published=%+v row=%+v", call, row)
			}
			wantScope := events.ScopeSession
			if tc.session == "" {
				wantScope = events.ScopeDaemon
			}
			if bus.published[0].Scope != wantScope {
				t.Fatalf("scope=%s", bus.published[0].Scope)
			}
		})
	}
}

func TestIngestFailureAndStartSemantics(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		phase                 string
		appendErr, publishErr error
		wantRows, wantPublish int
		wantMessage           string
	}{
		{name: "start", phase: ProxyEventPhaseStart, wantPublish: 1},
		{name: "append fails before publish", appendErr: errors.New("disk"), wantMessage: "persist proxy event: disk"},
		{name: "publish fails after append", publishErr: errors.New("bus"), wantRows: 1, wantPublish: 1, wantMessage: "publish tool call event: bus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &testRows{appendErr: tc.appendErr}
			bus := &testBus{err: tc.publishErr}
			err := New(st, bus, nil).Ingest(context.Background(), ProxyEventIngestRequest{ToolName: "tool", Phase: tc.phase, Publish: true, DurationMs: 7, OK: true, Error: "failure"}, func() callcontext.Snapshot { return callcontext.Snapshot{} })
			if (err == nil) != (tc.wantMessage == "") || (err != nil && err.Error() != tc.wantMessage) || len(st.rows) != tc.wantRows || len(bus.published) != tc.wantPublish {
				t.Fatalf("error=%v rows=%v publish=%v", err, st.rows, bus.published)
			}
			if tc.phase == ProxyEventPhaseStart {
				var call events.ToolCallEvent
				_ = json.Unmarshal([]byte(bus.published[0].PayloadJSON), &call)
				if call.DurationMs != 0 || call.OK || call.Error != "" || bus.published[0].Kind != events.EventTypeToolCallStart {
					t.Fatalf("start=%+v", call)
				}
			}
		})
	}
}
