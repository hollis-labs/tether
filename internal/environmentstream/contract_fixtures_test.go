package environmentstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// The files under docs/fixtures/managed-session are generated from the real
// types by this test, so the published contract examples cannot drift from the
// code. Run `UPDATE_FIXTURES=1 go test ./internal/environmentstream -run
// ContractFixtures` to rewrite them after an intentional change.
const fixtureDir = "../../docs/fixtures/managed-session"
const fixtureEnv = "5b3a1f0e-8c2d-4e6a-9f10-2d7c4b8a6e01"

var fixtureAt = time.Date(2026, 10, 21, 0, 12, 30, 0, time.UTC)

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(fixtureDir, name)
	if os.Getenv("UPDATE_FIXTURES") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s missing; regenerate with UPDATE_FIXTURES=1: %v", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("%s differs from the code's output; regenerate with UPDATE_FIXTURES=1 if the change is intended.\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}

func pretty(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func fixtureSnapshot() store.EnvironmentSnapshot {
	row := func(id, agent, state string) store.EnvironmentSessionRow {
		return store.EnvironmentSessionRow{ID: id, LogicalAgentID: agent, ProviderKind: "cli", State: state, UpdatedAt: fixtureAt.Add(-time.Minute).Format(time.RFC3339Nano), LastEventAt: fixtureAt.Format(time.RFC3339Nano)}
	}
	id := func(n string) string { return "00000000-0000-4000-8000-0000000000" + n }
	rows := []store.EnvironmentSessionRow{
		row(id("01"), "general", "running"),           // idle
		row(id("02"), "general", "running"),           // processing
		row(id("03"), "portfolio-manager", "running"), // awaiting approval
		row(id("04"), "portfolio-manager", "running"), // awaiting input
		row(id("05"), "general", "created"),           // starting
		row(id("06"), "general", "detached"),
		row(id("07"), "general", "orphaned"),
		row(id("08"), "general", "stopping"),
		row(id("09"), "general", "completed"),
		row(id("10"), "general", "failed"),
		row(id("11"), "general", "killed"),
		row(id("12"), "general", "running"), // work-state never observed
	}
	rows[4].RouteJSON = `{"channel":"ops","kinds":["final"]}`
	at := fixtureAt.Add(-30 * time.Second)
	return store.EnvironmentSnapshot{
		HighWater: 1312309, EarliestAvailable: 23836, Sessions: rows,
		Requests: []store.EnvironmentRequest{
			{SessionID: id("03"), TurnID: "t1", RequestID: "r1", Kind: "approval", Open: true, SourceSequence: 7},
			{SessionID: id("03"), TurnID: "t0", RequestID: "r0", Kind: "question", Open: false, SourceSequence: 3},
			{SessionID: id("04"), TurnID: "t1", RequestID: "r1", Kind: "question", Open: true, SourceSequence: 9},
			{SessionID: id("04"), TurnID: "t0", RequestID: "r0", Kind: "approval", Open: false, SourceSequence: 4},
		},
		WorkStates: map[string]store.EnvironmentWorkState{
			id("01"): {Value: "idle", At: at}, id("02"): {Value: "processing", At: at},
			id("03"): {Value: "processing", At: at}, id("04"): {Value: "processing", At: at},
			id("06"): {Value: "idle", At: at}, id("07"): {Value: "idle", At: at}, id("08"): {Value: "processing", At: at},
			id("09"): {Value: "idle", At: at},
		},
	}
}

func TestContractFixturesSnapshot(t *testing.T) {
	snap := snapshot(fixtureEnv, fixtureSnapshot())
	golden(t, "snapshot.json", pretty(t, snap))
	// The single-session snapshot has the same shape with exactly one session.
	one, ok := SessionFromSnapshot(fixtureSnapshot(), "00000000-0000-4000-8000-000000000003")
	if !ok {
		t.Fatal("fixture session missing")
	}
	golden(t, "session-awaiting-approval.json", pretty(t, one))
}

func fixtureEvent(t *testing.T, seq int64, kind, session, payload string) Event {
	t.Helper()
	e, err := Map(fixtureEnv, events.Event{Seq: seq, At: fixtureAt, Scope: events.ScopeSession, SessionID: session, Kind: kind, PayloadJSON: payload})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestContractFixturesEvents(t *testing.T) {
	const sid = "00000000-0000-4000-8000-000000000001"
	for name, e := range map[string]Event{
		"session.work_state_changed.json":    fixtureEvent(t, 1312310, events.KindSessionWorkStateChanged, sid, `{"from":"processing","to":"idle","at":"2026-10-21T00:12:30Z"}`),
		"session.state_changed.json":         fixtureEvent(t, 1312311, events.KindSessionStateChanged, sid, `{"from":"running","to":"completed","exit_code":0}`),
		"session.request_status.open.json":   fixtureEvent(t, 1312312, store.EnvironmentRequestKind, sid, `{"turn_id":"t1","request_id":"r1","request_kind":"approval","open":true,"source_sequence":7}`),
		"session.request_status.closed.json": fixtureEvent(t, 1312313, store.EnvironmentRequestKind, sid, `{"turn_id":"t1","request_id":"r1","request_kind":"approval","open":false,"source_sequence":8}`),
		"session.health_changed.json":        fixtureEvent(t, 1312314, events.KindSessionHealthChanged, sid, `{"from":"unknown","to":"healthy","reasons":["session_running"],"since":"2026-10-21T00:12:30Z"}`),
	} {
		golden(t, filepath.Join("events", name), pretty(t, e))
	}
}

// streamTranscript runs the real stream handler against one prepared window and
// records the SSE frames; a closed live feed ends it with the restart gap.
func streamTranscript(t *testing.T, window store.EnvironmentWindow, after string) []byte {
	t.Helper()
	live := make(chan events.Event)
	close(live)
	s := &Server{EnvironmentID: fixtureEnv, Store: &projectionWindows{windows: []store.EnvironmentWindow{window}}, Bus: &closingBus{live: live}, PollInterval: time.Millisecond}
	w := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Stream(w, httptest.NewRequest(http.MethodGet, "/environment/events?after_seq="+after, nil).WithContext(ctx), "")
	return []byte(strings.TrimSpace(w.Body.String()) + "\n")
}

func TestContractFixturesFrames(t *testing.T) {
	e := events.Event{Seq: 1312310, At: fixtureAt, Scope: events.ScopeSession, SessionID: "00000000-0000-4000-8000-000000000001", Kind: events.KindSessionWorkStateChanged, PayloadJSON: `{"from":"processing","to":"idle","at":"2026-10-21T00:12:30Z"}`}
	golden(t, "frames/event-synchronized-restart.sse", streamTranscript(t, store.EnvironmentWindow{HighWater: 1312310, EarliestAvailable: 23836, Events: []events.Event{e}}, "1312309"))
	golden(t, "frames/gap-purged.sse", streamTranscript(t, store.EnvironmentWindow{HighWater: 1312310, EarliestAvailable: 23836, GapReason: "purged"}, "100"))
	golden(t, "frames/gap-too-large.sse", streamTranscript(t, store.EnvironmentWindow{HighWater: 1312310, EarliestAvailable: 23836, GapReason: "too_large"}, "1000"))
	golden(t, "frames/gap-ahead.sse", streamTranscript(t, store.EnvironmentWindow{HighWater: 1312310, EarliestAvailable: 23836, GapReason: "ahead"}, "9999999"))
}

func compileSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	names := []string{"session", "snapshot", "event"}
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(fixtureDir, "schema", n+".schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if err := c.AddResource("https://tether.local/schemas/managed-session/"+n+".schema.json", doc); err != nil {
			t.Fatal(err)
		}
	}
	out := map[string]*jsonschema.Schema{}
	for _, n := range names {
		s, err := c.Compile("https://tether.local/schemas/managed-session/" + n + ".schema.json")
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = s
	}
	return out
}

func validateFile(t *testing.T, s *jsonschema.Schema, rel string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	if err := s.Validate(doc); err != nil {
		t.Errorf("%s does not validate: %v", rel, err)
	}
}

// Every published fixture validates against the published schema, the PROPOSED
// examples included; the schema rejects the dropped instance_id and malformed
// work-state values, so it is not vacuous.
func TestContractFixturesValidate(t *testing.T) {
	schemas := compileSchemas(t)
	validateFile(t, schemas["snapshot"], "snapshot.json")
	validateFile(t, schemas["session"], "session-awaiting-approval.json")
	validateFile(t, schemas["session"], "proposed/session-full.json")
	validateFile(t, schemas["event"], "proposed/session.projection_changed.json")
	events, err := filepath.Glob(filepath.Join(fixtureDir, "events", "*.json"))
	if err != nil || len(events) < 5 {
		t.Fatalf("event fixtures: %v %v", events, err)
	}
	for _, f := range events {
		rel, _ := filepath.Rel(fixtureDir, f)
		validateFile(t, schemas["event"], rel)
	}
	frames, _ := filepath.Glob(filepath.Join(fixtureDir, "frames", "*.sse"))
	for _, f := range frames {
		raw, _ := os.ReadFile(f)
		for _, block := range strings.Split(strings.TrimSpace(string(raw)), "\n\n") {
			var kind, data string
			for _, line := range strings.Split(block, "\n") {
				switch {
				case strings.HasPrefix(line, "event: "):
					kind = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					data = strings.TrimPrefix(line, "data: ")
				}
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(data), &obj); err != nil || kind == "" {
				t.Errorf("%s: frame %q is not event+JSON data", f, block)
				continue
			}
			if kind != "gap" && kind != "synchronized" {
				validateRaw(t, schemas["event"], f, data)
			}
		}
	}
	for name, bad := range map[string]string{
		"instance_id is dropped":   `{"id":"a","instance_id":"x","kind":"cli","session_state":"running","instance_status":"running","instance_detail":{},"last_activity":"2026-10-21T00:00:00Z","pending_question":false,"pending_approval":false,"pending_known":false,"pending_question_known":false,"pending_approval_known":false}`,
		"work_state needs known":   `{"id":"a","kind":"cli","session_state":"running","instance_status":"running","instance_detail":{},"last_activity":"2026-10-21T00:00:00Z","pending_question":false,"pending_approval":false,"pending_known":false,"pending_question_known":false,"pending_approval_known":false,"work_state":{"value":"idle"}}`,
		"unknown work_state value": `{"id":"a","kind":"cli","session_state":"running","instance_status":"running","instance_detail":{},"last_activity":"2026-10-21T00:00:00Z","pending_question":false,"pending_approval":false,"pending_known":false,"pending_question_known":false,"pending_approval_known":false,"work_state":{"value":"busy","known":true}}`,
	} {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(bad))
		if err != nil {
			t.Fatal(err)
		}
		if schemas["session"].Validate(doc) == nil {
			t.Errorf("schema accepted: %s", name)
		}
	}
}

func validateRaw(t *testing.T, s *jsonschema.Schema, name, raw string) {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := s.Validate(doc); err != nil {
		t.Errorf("%s does not validate: %v", name, err)
	}
}
