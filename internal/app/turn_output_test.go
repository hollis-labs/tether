package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

func outputHarness(t *testing.T, route *launchprofile.Route) (*Service, *sessionTurnOutput) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "output.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	row := store.SessionRow{ID: "s1", LogicalAgentID: "agent", ProjectID: "project", State: "created", WorkstreamID: sql.NullString{String: "workstream", Valid: true}}
	plan := &launch.Plan{ProviderBrand: "codex", Route: route}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	rowPtr, err := db.GetSession(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Bus: events.NewBus(events.BusOptions{Persister: db}), Catalog: &config.Catalog{Providers: map[string]config.Provider{"codex": {ID: "codex", Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio}}}}
	t.Cleanup(svc.stopOutputRetries)
	svc.installTurnFeeds()
	return svc, svc.newSessionTurnOutput(*rowPtr, plan)
}

func outputEvents(t *testing.T, svc *Service) []events.TurnOutputEvent {
	t.Helper()
	evs, err := svc.Store.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []events.TurnOutputEvent
	for _, ev := range evs {
		if ev.Kind == events.KindSessionTurnOutput {
			var payload events.TurnOutputEvent
			if err := json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			out = append(out, payload)
		}
	}
	return out
}

func TestTurnOutputUnroutedExcerptAndNoMessage(t *testing.T) {
	svc, output := outputHarness(t, nil)
	text := strings.Repeat("a", 4095) + "世界"
	output.observeProvider(gopevents.Thinking{Text: "private reasoning"})
	output.observeProvider(gopevents.Delta{Text: text, Phase: "final"})
	output.observeProvider(gopevents.Done{})
	output.flush()
	evs := outputEvents(t, svc)
	if len(evs) != 1 {
		t.Fatalf("events: %+v", evs)
	}
	ev := evs[0]
	if ev.MessageID != "" || ev.Text != strings.Repeat("a", 4095) || !ev.TextTruncated || !utf8.ValidString(ev.Text) {
		t.Fatalf("excerpt: %+v", ev)
	}
	if ev.SessionID != "s1" || ev.LogicalAgentID != "agent" || ev.ProjectID != "project" || ev.WorkstreamID == "" || ev.TurnID == "" || ev.Confidence != turnoutput.ConfidenceExact {
		t.Fatalf("metadata: %+v", ev)
	}
	var n int
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unrouted durable storage: %d %v", n, err)
	}
}

func TestTurnOutputStagesSelectedKindOnlyAndKeepsFullText(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	text := strings.Repeat("完整输出", 2000)
	output.observeProvider(gopevents.Delta{Text: text, Phase: "final"})
	output.observeProvider(gopevents.Done{})
	output.observeProvider(gopevents.Error{Err: errors.New("failed")})
	evs := outputEvents(t, svc)
	if len(evs) != 2 || evs[0].MessageID == "" || evs[0].Text != "" || evs[1].MessageID != "" || evs[1].Kind != turnoutput.KindFailure {
		t.Fatalf("events: %+v", evs)
	}
	saved, err := svc.Store.StagedTurnOutput(context.Background(), evs[0].MessageID)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(saved.Payload, &body); err != nil || body.Text != text {
		t.Fatalf("full text length %d: %v", len(body.Text), err)
	}
	if saved.From.URN() != "msg://session/local/s1" || saved.Metadata["turn_id"] != evs[0].TurnID {
		t.Fatalf("attribution: %+v", saved)
	}
	var n int
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stored twice or unselected: %d %v", n, err)
	}
}

func TestTurnOutputPersistenceFailureNeverEmitsMessageID(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	if _, err := svc.Store.DB().Exec(`CREATE TRIGGER fail_stage BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL,'fixture stage failure'); END`); err != nil {
		t.Fatal(err)
	}
	output.observeProvider(gopevents.Delta{Text: "answer", Phase: "final"})
	output.observeProvider(gopevents.Done{})
	evs := outputEvents(t, svc)
	if len(evs) != 1 || evs[0].MessageID != "" || evs[0].Text != "answer" {
		t.Fatalf("false message id: %+v", evs)
	}
}

func TestTurnOutputEmptyFinalFailureAndInterrupted(t *testing.T) {
	svc, output := outputHarness(t, nil)
	output.observeProvider(gopevents.Done{})
	output.observeProvider(gopevents.Error{})
	output.observeProvider(gopevents.Delta{Text: "partial"})
	output.flush()
	output.flush()
	evs := outputEvents(t, svc)
	if len(evs) != 2 || evs[0].Kind != turnoutput.KindFailure || evs[1].Kind != turnoutput.KindTerminal || evs[1].StopReason != "error" {
		t.Fatalf("terminal outputs: %+v", evs)
	}
}

func TestEveryNativeLaunchHasChainedTypedCallback(t *testing.T) {
	for _, brand := range []string{"claude", "codex", "opencode", "antigravity"} {
		t.Run(brand, func(t *testing.T) {
			svc, rt, id, _ := credentialLaunch(t, brand)
			svc.Bus = events.NewBus(events.BusOptions{Persister: svc.Store})
			if brand == "opencode" || brand == "antigravity" {
				plan, err := svc.Store.GetLaunchPlan(id)
				if err != nil {
					t.Fatal(err)
				}
				plan.RuntimeKind = "subprocess"
				data, _ := json.Marshal(plan)
				if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(data), id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.LaunchSession(id); err != nil {
				t.Fatal(err)
			}
			if rt.options.TypedEventCallback == nil {
				t.Fatal("typed callback not wired")
			}
			rt.options.TypedEventCallback(gopevents.Delta{Text: "reply", Phase: "final"})
			rt.options.TypedEventCallback(gopevents.PermissionDenied{Action: "write", DisplayName: "file"})
			rt.options.TypedEventCallback(gopevents.Done{})
			evs := outputEvents(t, svc)
			if len(evs) != 1 || evs[0].Runtime != brand {
				t.Fatalf("runtime output: %+v", evs)
			}
			all, _ := svc.Store.EventsSince(0)
			found := false
			for _, ev := range all {
				found = found || ev.Kind == events.KindProviderPermissionDenied
			}
			if found != (brand == "antigravity") {
				t.Fatalf("legacy permission event changed for runtime %s: %v", brand, found)
			}
		})
	}
}
func TestTurnOutputRuntimeFeedAndCallbackChaining(t *testing.T) {
	svc, output := outputHarness(t, nil)
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindAgentDelta, TurnID: "turn-1", Payload: []byte(`{"content":"ACP reply","phase":"final"}`)})
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "turn-1"})
	evs := outputEvents(t, svc)
	if len(evs) != 1 || evs[0].TurnID != "turn-1" || evs[0].Text != "ACP reply" {
		t.Fatalf("runtime feed: %+v", evs)
	}
	called := false
	opts := agentsessions.StartOptions{TypedEventCallback: func(gopevents.Event) { called = true }}
	output.wire(nil, &opts)
	opts.TypedEventCallback(gopevents.Done{})
	if !called {
		t.Fatal("existing callback not chained")
	}
}

func TestStagedOutputNeverWakesSession(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	output.observeProvider(gopevents.Delta{Text: "answer", Phase: "final"})
	output.observeProvider(gopevents.Done{})
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	attempted, err := runWakeSweep(context.Background(), svc.Store, nil, rt.seam(), nil)
	if err != nil || attempted != 0 || rt.sendCallCount("s1") != 0 {
		t.Fatalf("staged wake: %d %v", attempted, err)
	}
}

func TestTurnOutputFlushAtWholeSessionExit(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	svc.Bus = events.NewBus(events.BusOptions{Persister: svc.Store})
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	rt.options.TypedEventCallback(gopevents.Delta{Text: "unfinished reply"})
	if err := svc.Manager.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		outputs := outputEvents(t, svc)
		if len(outputs) > 0 {
			if len(outputs) != 1 || outputs[0].Kind != turnoutput.KindTerminal || outputs[0].Text != "unfinished reply" {
				t.Fatalf("exit outputs: %+v", outputs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("whole-session exit did not flush partial output")
}

func TestTurnOutputQuestionApprovalAndEmptySelectedKinds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kinds  []string
		event  gopevents.Event
		want   turnoutput.Kind
		staged bool
	}{
		{"question", []string{"question"}, gopevents.ToolUse{Name: "AskUserQuestion", Args: map[string]any{"question": "Choose a target?"}}, turnoutput.KindQuestion, true},
		{"approval", []string{"approval"}, gopevents.PermissionDenied{Action: "write", DisplayName: "file"}, turnoutput.KindApproval, true},
		{"disabled kinds", []string{}, gopevents.PermissionDenied{Action: "write", DisplayName: "file"}, turnoutput.KindApproval, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: tc.kinds})
			output.observeProvider(tc.event)
			output.observeProvider(gopevents.Done{})
			evs := outputEvents(t, svc)
			if len(evs) != 1 || evs[0].Kind != tc.want || (evs[0].MessageID != "") != tc.staged {
				t.Fatalf("kind selection: %+v", evs)
			}
		})
	}
}
