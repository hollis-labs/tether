package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

type metadataErrorStore struct{ *store.Store }

func (s metadataErrorStore) GetSessionContext(context.Context, string) (*store.SessionRow, error) {
	return nil, errors.New("metadata unavailable")
}

func TestNonContextMetadataErrorStillPublishesUnroutedOutput(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputStore = metadataErrorStore{svc.Store}
	output.observeProvider(gopevents.Done{Text: "answer"})
	got := outputEvents(t, svc)
	if len(got) != 1 || got[0].Text != "answer" || got[0].WorkstreamID != "" {
		t.Fatalf("metadata failure deferred output: %+v", got)
	}
	svc.outputRetries.mu.Lock()
	count := svc.outputRetries.count
	svc.outputRetries.mu.Unlock()
	if count != 0 {
		t.Fatal("non-context metadata error queued a retry")
	}
}

type slowMetadataStore struct {
	*store.Store
	metadataDeadline time.Time
	stageDeadline    time.Time
}

func (s *slowMetadataStore) GetSessionContext(ctx context.Context, id string) (*store.SessionRow, error) {
	s.metadataDeadline, _ = ctx.Deadline()
	time.Sleep(20 * time.Millisecond)
	return &store.SessionRow{ID: id}, nil
}
func (s *slowMetadataStore) StageTurnOutput(ctx context.Context, _ messaging.Envelope) (messaging.Envelope, error) {
	s.stageDeadline, _ = ctx.Deadline()
	<-ctx.Done()
	return messaging.Envelope{}, ctx.Err()
}
func TestSynchronousOutputSharesOneDeadline(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	svc.turnOutputTimeout = 60 * time.Millisecond
	storage := &slowMetadataStore{Store: svc.Store}
	job := &turnOutputWrite{storage: storage, row: store.SessionRow{ID: "s1"}, route: &launchprofile.Route{Kinds: []string{"final"}}}
	job.result.Kind = "final"
	ctx, cancel := svc.outputPersistenceContext()
	defer cancel()
	if err := job.persist(ctx, svc); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if storage.stageDeadline.After(storage.metadataDeadline) {
		t.Fatalf("stage extended overall deadline: metadata=%s stage=%s", storage.metadataDeadline, storage.stageDeadline)
	}
}

type failTwiceOutputBus struct {
	events.Bus
	calls    atomic.Int32
	retryAt  atomic.Int64
	retryGap atomic.Int64
}

func (b *failTwiceOutputBus) Publish(ctx context.Context, ev events.Event) error {
	call := b.calls.Add(1)
	if call == 2 {
		b.retryAt.Store(time.Now().UnixNano())
	}
	if call == 3 {
		b.retryGap.Store(time.Now().UnixNano() - b.retryAt.Load())
	}
	if call <= 2 {
		return errors.New("temporary event failure")
	}
	return b.Bus.Publish(ctx, ev)
}
func TestOutputRetrySurvivesAnotherFailure(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	bus := &failTwiceOutputBus{Bus: svc.Bus}
	svc.Bus = bus
	output.observeProvider(gopevents.Done{Text: "eventual answer"})
	deadline := time.After(3 * time.Second)
	for len(outputEvents(t, svc)) == 0 {
		select {
		case <-deadline:
			t.Fatal("second retry did not succeed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	svc.stopOutputRetries()
	if bus.calls.Load() != 3 {
		t.Fatal("unexpected retry count", bus.calls.Load())
	}
	if time.Duration(bus.retryGap.Load()) < 180*time.Millisecond {
		t.Fatal("retry did not back off", time.Duration(bus.retryGap.Load()))
	}
	var count int
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal("restaged body", count, err)
	}
}
func TestOutputRetryFinalAttemptAtStop(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	bus := &failTwiceOutputBus{Bus: svc.Bus}
	bus.calls.Store(1)
	svc.Bus = bus
	output.observeProvider(gopevents.Done{Text: "shutdown answer"})
	svc.stopOutputRetries()
	got := outputEvents(t, svc)
	if len(got) != 1 || got[0].MessageID == "" {
		t.Fatalf("stop dropped pending event: %+v", got)
	}
	if bus.calls.Load() != 3 {
		t.Fatal("missing final attempt", bus.calls.Load())
	}
}

type operationStallStore struct {
	*store.Store
	operation string
	calls     atomic.Int32
	timedOut  atomic.Bool
}

func (s *operationStallStore) SessionRoute(ctx context.Context, id string) (*launchprofile.Route, error) {
	if s.operation == "route" && s.calls.Add(1) == 1 {
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		route, err := s.Store.SessionRoute(ctx, id)
		s.timedOut.Store(errors.Is(err, context.DeadlineExceeded))
		return route, err
	}
	return s.Store.SessionRoute(ctx, id)
}
func (s *operationStallStore) StageTurnOutput(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	if s.operation == "stage" && s.calls.Add(1) == 1 {
		conn, err := s.DB().Conn(ctx)
		if err != nil {
			return messaging.Envelope{}, err
		}
		defer conn.Close()
		saved, err := s.Store.StageTurnOutput(ctx, env)
		s.timedOut.Store(errors.Is(err, context.DeadlineExceeded))
		return saved, err
	}
	return s.Store.StageTurnOutput(ctx, env)
}
func TestOutputRetriesIsolatedRouteAndStageStalls(t *testing.T) {
	for _, operation := range []string{"route", "stage"} {
		t.Run(operation, func(t *testing.T) {
			svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
			svc.turnOutputTimeout = 25 * time.Millisecond
			storage := &operationStallStore{Store: svc.Store, operation: operation}
			svc.turnOutputStore = storage
			if operation == "route" {
				output.route = nil
				output.routeUnread = true
			}
			output.observeProvider(gopevents.Delta{Text: "answer"})
			marker, done := output.CurrentTurn()
			output.observeProvider(gopevents.Done{})
			if !storage.timedOut.Load() {
				t.Fatal("target operation did not time out")
			}
			select {
			case <-done:
			default:
				t.Fatal("stall retained turn marker")
			}
			if _, ok := output.CompletedTurn(marker); !ok {
				t.Fatal("completion lost")
			}
			deadline := time.After(3 * time.Second)
			for {
				got := outputEvents(t, svc)
				if len(got) == 1 {
					if got[0].MessageID == "" {
						t.Fatal("retry lost routed body")
					}
					break
				}
				select {
				case <-deadline:
					t.Fatal("output not recovered")
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	}
}
func TestAmbiguityResetsAfterBoundTurnSettles(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	output.observeProvider(gopevents.Done{}) // Prime empty-terminal suppression.
	err := svc.trackTurnSubmission("s1", func() error { return svc.trackTurnSubmission("s1", func() error { return nil }) })
	if err != nil {
		t.Fatal(err)
	}
	output.observeProvider(gopevents.Done{})
	if id, _ := output.CurrentTurn(); id == "" {
		t.Fatal("ambiguous empty turn unexpectedly settled")
	}
	output.observeProvider(gopevents.Done{Text: "bound answer"})
	output.observeProvider(gopevents.Done{}) // Prime the next repeated empty terminal.
	var marker string
	if err := svc.trackTurnSubmission("s1", func() error { marker, _ = output.CurrentTurn(); output.observeProvider(gopevents.Done{}); return nil }); err != nil {
		t.Fatal(err)
	}
	if completion, ok := output.CompletedTurnDetails(marker); !ok || !completion.Synthetic {
		t.Fatalf("settled ambiguity poisoned next empty turn: %+v %v", completion, ok)
	}
}

type reorderedOutputBus struct {
	events.Bus
	release atomic.Bool
}

func (b *reorderedOutputBus) Publish(ctx context.Context, ev events.Event) error {
	var payload events.TurnOutputEvent
	if err := json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
		return err
	}
	if payload.Text == "first" && !b.release.Load() {
		return errors.New("first output delayed")
	}
	err := b.Bus.Publish(ctx, ev)
	if payload.Text == "second" && err == nil {
		b.release.Store(true)
	}
	return err
}
func TestOutputEventsCanReorderAcrossRetry(t *testing.T) {
	svc, output := outputHarness(t, nil)
	bus := &reorderedOutputBus{Bus: svc.Bus}
	svc.Bus = bus
	output.observeProvider(gopevents.Done{Text: "first"})
	output.observeProvider(gopevents.Done{Text: "second"})
	deadline := time.After(3 * time.Second)
	for {
		got := outputEvents(t, svc)
		if len(got) == 2 {
			if got[0].Text != "second" || got[1].Text != "first" || got[0].TurnID == got[1].TurnID {
				t.Fatalf("unexpected event order: %+v", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("delayed output not published")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Non-context failures after each stall allow the reader to reach all four
// operations. Returning ctx.Err on metadata would mask a missing shared cap.
type everyOperationStallStore struct {
	*store.Store
	entered chan struct{}
	first   atomic.Bool
}

func (s *everyOperationStallStore) wait(ctx context.Context) {
	if s.first.CompareAndSwap(false, true) {
		close(s.entered)
	}
	<-ctx.Done()
}
func (s *everyOperationStallStore) GetSessionContext(ctx context.Context, _ string) (*store.SessionRow, error) {
	s.wait(ctx)
	return nil, errors.New("metadata unavailable")
}
func (s *everyOperationStallStore) SessionRoute(ctx context.Context, _ string) (*launchprofile.Route, error) {
	s.wait(ctx)
	return nil, errors.New("route unavailable")
}
func (s *everyOperationStallStore) StageTurnOutput(ctx context.Context, _ messaging.Envelope) (messaging.Envelope, error) {
	s.wait(ctx)
	return messaging.Envelope{}, errors.New("stage unavailable")
}

type everyOperationStallBus struct{ events.Bus }

func (b everyOperationStallBus) Publish(ctx context.Context, _ events.Event) error {
	<-ctx.Done()
	return ctx.Err()
}
func TestObserveProviderBoundsReaderLockAcrossAllPersistenceStalls(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	budget := 200 * time.Millisecond
	svc.turnOutputTimeout = budget
	storage := &everyOperationStallStore{Store: svc.Store, entered: make(chan struct{})}
	svc.turnOutputStore = storage
	svc.Bus = everyOperationStallBus{svc.Bus}
	output.routeUnread = true
	output.observeProvider(gopevents.Delta{Text: "answer"})
	start := time.Now()
	returned := make(chan time.Duration, 1)
	go func() { output.observeProvider(gopevents.Done{}); returned <- time.Since(start) }()
	select {
	case <-storage.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never entered persistence")
	}
	// This must regain the same mutex held by observeProvider, after every
	// synchronous persistence attempt and marker completion has finished.
	output.CurrentTurn()
	held := <-returned
	if held >= 2*budget {
		t.Fatalf("reader lock held %s for %s overall cap", held, budget)
	}
}
