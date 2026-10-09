package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tether/internal/events"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gopevents "github.com/hollis-labs/substrate/harness/adapters/provider/events"
	"github.com/hollis-labs/substrate/harness/adapters/runtimeevents"
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

func TestBlockedPersistenceRetriesOutputWithoutFreezingTurnCompletion(t *testing.T) {
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
	deadline := time.After(3 * time.Second)
	for {
		got := outputEvents(t, svc)
		if len(got) == 1 {
			if got[0].MessageID == "" {
				t.Fatal("routed output lost its body")
			}
			env, err := svc.Store.StagedTurnOutput(context.Background(), got[0].MessageID)
			if err != nil || string(env.Payload) != `{"text":"answer"}` {
				t.Fatalf("lost body: %+v %v", env, err)
			}
			svc.stopOutputRetries()
			var count int
			if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("duplicate stage: %d %v", count, err)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("output was not eventually staged/published: %+v", got)
		case <-time.After(10 * time.Millisecond):
		}
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
	text := strings.Repeat("unroutable", 600)
	output.observeProvider(gopevents.Done{Text: text})
	if len(outputEvents(t, svc)) != 0 {
		t.Fatal("corrupt route invented an unrouted publication")
	}
	ids, err := svc.Store.PendingTurnOutputRetries("", 128)
	if err != nil || len(ids) != 1 {
		t.Fatal("corrupt route lost its pending output", err)
	}
	journal, err := readOutputRetry(svc, ids[0])
	if err != nil || journal.result.Text != text || !journal.routeUnread {
		t.Fatal("corrupt route did not retain the full body", err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET route_json='{"channel":"ops","kinds":["final"]}' WHERE id='s1'`); err != nil {
		t.Fatal(err)
	}
	first := waitOutputEvents(t, svc, 1)[0]
	if first.MessageID == "" {
		t.Fatal("corrected route did not recover selected output")
	}
	env, err := svc.Store.StagedTurnOutput(context.Background(), first.MessageID)
	var body struct {
		Text string `json:"text"`
	}
	if err != nil || json.Unmarshal(env.Payload, &body) != nil || body.Text != text {
		t.Fatal("corrected route lost its retained full body", err)
	}
	output.observeProvider(gopevents.Done{Text: "recovered"})
	if got := waitOutputEvents(t, svc, 2); len(got) != 2 || got[1].MessageID == "" {
		t.Fatal("route not retried for later output")
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
	if _, ok := output.CompletedTurnDetails(oldest); ok {
		t.Fatal("old detailed completion retained")
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

func TestFailedCreatorCannotSettlePendingSteering(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	failFirst := make(chan struct{})
	failSecond := make(chan struct{})
	firstReturned := make(chan error, 1)
	secondReturned := make(chan error, 1)
	go func() {
		firstReturned <- svc.trackTurnSubmission("s1", func() error { close(firstEntered); <-failFirst; return errors.New("first failure") })
	}()
	<-firstEntered
	expected, done := output.CurrentTurn()
	go func() {
		secondReturned <- svc.trackTurnSubmission("s1", func() error { close(secondEntered); <-failSecond; return errors.New("second failure") })
	}()
	<-secondEntered
	close(failFirst)
	if err := <-firstReturned; err == nil {
		t.Fatal("first error hidden")
	}
	id, _ := output.CurrentTurn()
	if id != expected || id == "" {
		close(failSecond)
		<-secondReturned
		t.Fatal("failed creator settled pending steering")
	}
	select {
	case <-done:
		close(failSecond)
		<-secondReturned
		t.Fatal("pending steering completed")
	default:
	}
	close(failSecond)
	if err := <-secondReturned; err == nil {
		t.Fatal("second error hidden")
	}
	select {
	case <-done:
	default:
		t.Fatal("all rejected submissions retained marker")
	}
	if id, _ := output.CurrentTurn(); id != "" {
		t.Fatal("rejected marker still current")
	}
}

// Fail publication after staging, then allow the retry to reach the real bus.
type stalledOutputBus struct {
	events.Bus
	calls atomic.Int32
}

func (b *stalledOutputBus) Publish(ctx context.Context, ev events.Event) error {
	if b.calls.Add(1) == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.Bus.Publish(ctx, ev)
}
func TestTurnOutputEventRetryRetainsSingleStagedBody(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	svc.turnOutputTimeout = 25 * time.Millisecond
	svc.Bus = &stalledOutputBus{Bus: svc.Bus}
	output.observeProvider(gopevents.Done{Text: "retry body"})
	deadline := time.After(3 * time.Second)
	for {
		got := outputEvents(t, svc)
		if len(got) == 1 {
			svc.stopOutputRetries()
			var count int
			if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("restaged output: %d %v", count, err)
			}
			if got[0].MessageID == "" {
				t.Fatal("retry dropped message id")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("event retry failed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestACPCompletedOutOfOrderTurnsRejectLateFrames(t *testing.T) {
	_, output := outputHarness(t, nil)
	for _, id := range []string{"first", "second"} {
		output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: id})
	}
	for _, id := range []string{"first", "second"} {
		output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: id})
	}
	if id, _ := output.CurrentTurn(); id != "" {
		t.Fatalf("completed turn retained %q", id)
	}
	for _, kind := range []runtimeevents.EventKind{runtimeevents.KindTurnStarted, runtimeevents.KindAgentDelta, runtimeevents.KindTurnCompleted} {
		output.observeRuntime(runtimeevents.Event{Kind: kind, TurnID: "first", Payload: []byte(`{"text":"late"}`)})
		if id, _ := output.CurrentTurn(); id != "" {
			t.Fatalf("late %s reopened phantom marker %q", kind, id)
		}
	}
	// The guard applies only to completed turns; a new turn still opens.
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "third"})
	if id, _ := output.CurrentTurn(); id == "" {
		t.Fatal("new turn blocked by completion guard")
	}
}

func TestInterruptAuditStorageDeadline(t *testing.T) {
	for _, viaBus := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct store", true: "bus"}[viaBus], func(t *testing.T) {
			svc, _ := outputHarness(t, nil)
			svc.turnOutputTimeout = 25 * time.Millisecond
			if !viaBus {
				svc.Bus = nil
			}
			conn, err := svc.Store.DB().Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			returned := make(chan error, 1)
			go func() {
				returned <- svc.auditTurnInterrupt(events.KindSessionTurnOutput, events.TurnInterruptEvent{SessionID: "s1"})
			}()
			select {
			case err := <-returned:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("audit error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("audit ignored persistence budget")
			}
		})
	}
}

func TestOutputStorageOperationsHonorTheirContexts(t *testing.T) {
	svc, _ := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	conn, err := svc.Store.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, op := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"route reread", func(ctx context.Context) error { _, err := svc.Store.SessionRoute(ctx, "s1"); return err }},
		{"event insert", func(ctx context.Context) error {
			_, _, err := svc.Store.InsertEventContext(ctx, events.ScopeSession, "s1", events.KindSessionTurnOutput, `{}`)
			return err
		}},
	} {
		t.Run(op.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			returned := make(chan error, 1)
			go func() { returned <- op.run(ctx) }()
			select {
			case err := <-returned:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("context ignored: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("storage operation stuck")
			}
		})
	}
}

type joiningOutputBus struct {
	events.Bus
	calls   atomic.Int32
	entered chan struct{}
	checked chan error
	svc     *Service
}

func (b *joiningOutputBus) Publish(ctx context.Context, ev events.Event) error {
	call := b.calls.Add(1)
	if call > 2 {
		return b.Bus.Publish(ctx, ev)
	}
	if call == 1 {
		return context.DeadlineExceeded
	}
	close(b.entered)
	<-ctx.Done()
	b.checked <- b.svc.Store.DB().PingContext(context.Background())
	return ctx.Err()
}
func TestServiceCloseJoinsOutputRetryBeforeClosingStorage(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputTimeout = 25 * time.Millisecond
	bus := &joiningOutputBus{Bus: svc.Bus, entered: make(chan struct{}), checked: make(chan error, 1), svc: svc}
	svc.Bus = bus
	output.observeProvider(gopevents.Done{Text: "retry at shutdown"})
	select {
	case <-bus.entered:
	case <-time.After(time.Second):
		t.Fatal("retry did not enter bus")
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-bus.checked; err != nil {
		t.Fatal("storage closed before retry joined", err)
	}
}
