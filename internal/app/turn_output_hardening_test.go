package app

import (
	"context"
	"errors"
	"testing"
	"time"

	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

func TestTurnOutputReadsCurrentWorkstreamAtPublication(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	if _, err := svc.Store.CreateWorkstream(store.WorkstreamRow{ID: "new-work"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.AssignSessionWorkstream("s1", "new-work"); err != nil {
		t.Fatal(err)
	}
	output.observeProvider(gopevents.Done{Text: "first"})
	first := outputEvents(t, svc)[0]
	env, err := svc.Store.StagedTurnOutput(context.Background(), first.MessageID)
	if err != nil || first.WorkstreamID != "new-work" || env.Metadata["workstream_id"] != "new-work" {
		t.Fatalf("stale metadata: %+v %+v %v", first, env, err)
	}
	if err := svc.Store.AssignSessionWorkstream("s1", ""); err != nil {
		t.Fatal(err)
	}
	output.observeProvider(gopevents.Done{Text: "second"})
	second := outputEvents(t, svc)[1]
	env, err = svc.Store.StagedTurnOutput(context.Background(), second.MessageID)
	if err != nil || second.WorkstreamID != "" || env.Metadata["workstream_id"] != "" {
		t.Fatalf("assignment not cleared: %+v %+v %v", second, env, err)
	}
}

func TestBlockedPersistenceCannotFreezeTurnCompletion(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	svc.turnOutputTimeout = 25 * time.Millisecond
	output.observeProvider(gopevents.Delta{Text: "answer"})
	id, done := output.CurrentTurn()
	conn, err := svc.Store.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	returned := make(chan struct{})
	go func() { output.observeProvider(gopevents.Done{}); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("reader frozen by storage")
	}
	select {
	case <-done:
	default:
		t.Fatal("blocked persistence retained marker")
	}
	if completed, ok := output.CompletedTurn(id); !ok || completed != id {
		t.Fatal("completion lost")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := outputEvents(t, svc); len(got) != 0 {
		t.Fatalf("failed persist emitted event: %+v", got)
	}
}

func TestCorruptRouteIsRetriedWithoutInventingDefaults(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET route_json='{"channel":"ops"}' WHERE id='s1'`); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	output := svc.newSessionTurnOutput(*row, &launch.Plan{ProviderBrand: "codex"})
	output.observeProvider(gopevents.Done{Text: "unroutable"})
	first := outputEvents(t, svc)[0]
	if first.MessageID != "" || first.Text != "unroutable" {
		t.Fatalf("invented route: %+v", first)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET route_json='{"channel":"ops","kinds":["final"]}' WHERE id='s1'`); err != nil {
		t.Fatal(err)
	}
	output.observeProvider(gopevents.Done{Text: "recovered"})
	if got := outputEvents(t, svc); len(got) != 2 || got[1].MessageID == "" {
		t.Fatalf("route not retried: %+v", got)
	}
}

func TestFailedCreatorPreservesAcceptedSteeringMarker(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	var expected string
	err := svc.trackTurnSubmission("s1", func() error {
		expected, _ = output.CurrentTurn()
		if err := svc.trackTurnSubmission("s1", func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		return errors.New("creator failed")
	})
	if err == nil {
		t.Fatal("failure hidden")
	}
	if id, _ := output.CurrentTurn(); id != expected || !output.TurnAccepted(id) {
		t.Fatal("accepted steering was settled")
	}
}

func TestLateACPCompletionDoesNotRebindSuccessor(t *testing.T) {
	_, output := outputHarness(t, nil)
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "first"})
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "second"})
	successor, done := output.CurrentTurn()
	if !output.TurnAccepted(successor) {
		t.Fatal("runtime start did not accept the marker")
	}
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "first"})
	if id, _ := output.CurrentTurn(); id != successor {
		t.Fatal("late terminal rebound successor")
	}
	select {
	case <-done:
		t.Fatal("late terminal settled successor")
	default:
	}
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "second"})
	if id, ok := output.CompletedTurn(successor); !ok || id != "second" {
		t.Fatal("successor completion absent")
	}
}

func TestTurnCompletionRecordsExpireAfterSixtyFourOutputs(t *testing.T) {
	_, output := outputHarness(t, nil)
	var oldest, newest string
	for i := 0; i < 65; i++ {
		output.observeProvider(gopevents.Delta{Text: "answer"})
		id, _ := output.CurrentTurn()
		if i == 0 {
			oldest = id
		}
		newest = id
		output.observeProvider(gopevents.Done{})
	}
	if _, ok := output.CompletedTurn(oldest); ok {
		t.Fatal("old completion retained without bound")
	}
	if id, ok := output.CompletedTurn(newest); !ok || id != newest {
		t.Fatal("latest completion evicted")
	}
}

func TestTurnCollectorRemovedOnLaunchFailureAndSessionExit(t *testing.T) {
	for _, fail := range []bool{true, false} {
		name := "exit"
		if fail {
			name = "start failure"
		}
		t.Run(name, func(t *testing.T) {
			svc, rt, id, _ := credentialLaunch(t)
			rt.fail = fail
			_, err := svc.LaunchSession(id)
			if fail {
				if err == nil {
					t.Fatal("launch succeeded")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := svc.SessionTurnOutputState(id); !ok {
					t.Fatal("collector never installed")
				}
				if err := svc.Manager.Stop(context.Background(), id); err != nil {
					t.Fatal(err)
				}
			}
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for {
				if _, ok := svc.SessionTurnOutputState(id); !ok {
					return
				}
				select {
				case <-ticker.C:
				case <-deadline.C:
					t.Fatal("terminal collector leaked")
				}
			}
		})
	}
}
