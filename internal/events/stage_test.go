package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// envPersister stores the whole Event it is given, like the SQLite persister.
type envPersister struct {
	fakePersister
	stored []Event
}

func (p *envPersister) InsertEventEnvelope(_ context.Context, e Event) (Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	e.Seq, e.At, e.EventID = p.seq, time.Now().UTC(), "evt_fake"
	p.stored = append(p.stored, e)
	return e, nil
}

func (p *envPersister) last() Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stored[len(p.stored)-1]
}

func setAgent(urn string) Stage {
	return StageFunc(func(_ context.Context, e *Event) error {
		if e.Envelope.Dims.AgentURN == "" {
			e.Envelope.Dims.AgentURN = urn
		}
		return nil
	})
}

func TestStagesRunInOrderBeforeThePersist(t *testing.T) {
	p := &envPersister{}
	var order []int
	mk := func(n int) Stage {
		return StageFunc(func(_ context.Context, e *Event) error {
			order = append(order, n)
			e.Envelope.Corr.CorrelationID += string(rune('a' + n))
			return nil
		})
	}
	bus := NewBus(BusOptions{Persister: p, Stages: []Stage{mk(0), mk(1), mk(2)}})
	if err := bus.Publish(context.Background(), Event{Scope: ScopeSession, Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	if got := p.last().Envelope.Corr.CorrelationID; got != "abc" || len(order) != 3 {
		t.Fatalf("correlation %q, order %v", got, order)
	}
}

func TestStageDoesNotOverwriteWhatTheProducerSet(t *testing.T) {
	p := &envPersister{}
	bus := NewBus(BusOptions{Persister: p, Stages: []Stage{setAgent("msg://agent/stage/x")}})
	e := Event{Scope: ScopeSession, Kind: "k"}
	e.Envelope.Dims.AgentURN = "msg://agent/producer/y"
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := p.last().Envelope.Dims.AgentURN; got != "msg://agent/producer/y" {
		t.Fatalf("producer value overwritten: %q", got)
	}
}

func TestFailingOrPanickingStageNeverLosesTheEvent(t *testing.T) {
	p := &envPersister{}
	var mu sync.Mutex
	var reported []*StageError
	boom := StageFunc(func(_ context.Context, e *Event) error {
		e.Envelope.Dims.ProjectID = "half-stamped" // must not persist
		return errors.New("lookup failed")
	})
	panics := StageFunc(func(_ context.Context, e *Event) error {
		e.Envelope.Dims.TaskID = "half-stamped"
		panic("nil session cache")
	})
	bus := NewBus(BusOptions{Persister: p, Stages: []Stage{boom, panics, setAgent("msg://agent/ok/z")}, OnStageError: func(err *StageError) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, err)
	}})
	if err := bus.Publish(context.Background(), Event{Scope: ScopeSession, Kind: "k"}); err != nil {
		t.Fatalf("publish failed because a stage did: %v", err)
	}
	got := p.last()
	if got.Envelope.Dims.ProjectID != "" || got.Envelope.Dims.TaskID != "" {
		t.Fatalf("a failed stage's partial work persisted: %+v", got.Envelope.Dims)
	}
	if got.Envelope.Dims.AgentURN != "msg://agent/ok/z" {
		t.Fatal("a later stage was skipped after an earlier one failed")
	}
	if len(reported) != 2 || reported[0].Stage != 0 || reported[1].Stage != 1 || reported[0].Kind != "k" {
		t.Fatalf("reported: %v", reported)
	}
}

type txSafeStage struct{ calls *int }

func (s txSafeStage) Stamp(_ context.Context, e *Event) error {
	*s.calls++
	e.Envelope.Dims.Host = "tx-safe"
	return nil
}
func (txSafeStage) TxSafe() bool { return true }

func TestPublishTxRunsOnlyTxSafeStages(t *testing.T) {
	p := &fakeTxPersister{}
	var safeCalls, otherCalls int
	other := StageFunc(func(context.Context, *Event) error { otherCalls++; return nil })
	bus := NewBus(BusOptions{Persister: p, Stages: []Stage{other, txSafeStage{&safeCalls}}}).(OutboxBus)
	if _, err := bus.PublishTx(context.Background(), nil, Event{Scope: ScopeSession, Kind: "k"}); err != nil {
		t.Fatal(err)
	}
	if safeCalls != 1 || otherCalls != 0 {
		t.Fatalf("safe=%d other=%d: a stage that may read the database ran inside a caller's tx", safeCalls, otherCalls)
	}
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.pending[0].Envelope.Dims.Host != "tx-safe" {
		t.Fatal("tx-safe stage output not persisted")
	}
}

func TestNoStagesLeavesTheEventUntouched(t *testing.T) {
	p := &envPersister{}
	bus := NewBus(BusOptions{Persister: p})
	e := Event{Scope: ScopeSession, Kind: "k"}
	e.Envelope.Dims.WorkerID = "w"
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if got := p.last().Envelope; got.Dims.WorkerID != "w" || got.Dims.AgentURN != "" {
		t.Fatalf("envelope changed: %+v", got)
	}
}

func TestFailedStageCannotLeaveALabelMutationBehind(t *testing.T) {
	for name, mutate := range map[string]func(e *Event){
		"in place": func(e *Event) { e.Envelope.Class.Labels[0] = "x" },
		"append":   func(e *Event) { e.Envelope.Class.Labels = append(e.Envelope.Class.Labels, "y") },
	} {
		t.Run(name, func(t *testing.T) {
			p := &envPersister{}
			bad := StageFunc(func(_ context.Context, e *Event) error {
				mutate(e)
				return errors.New("then fails")
			})
			bus := NewBus(BusOptions{Persister: p, Stages: []Stage{bad}, OnStageError: func(*StageError) {}})
			// Spare capacity makes an append write into the shared backing array.
			labels := make([]string, 1, 4)
			labels[0] = "a"
			e := Event{Scope: ScopeSession, Kind: "k"}
			e.Envelope.Class.Labels = labels
			if err := bus.Publish(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			got := p.last().Envelope.Class.Labels
			if len(got) != 1 || got[0] != "a" {
				t.Fatalf("published labels %v, want [a]", got)
			}
			if labels[0] != "a" || len(labels) != 1 || labels[:2][1] != "" {
				t.Fatalf("the producer's slice was written through: %v %v", labels, labels[:2])
			}
		})
	}
}

func TestFailedStageCannotMutateDelegationOrRawOffset(t *testing.T) {
	p := &envPersister{}
	bad := StageFunc(func(_ context.Context, e *Event) error {
		e.Envelope.Actor.OnBehalfOfChain[0] = "changed"
		e.Envelope.Actor.OnBehalfOfChain = append(e.Envelope.Actor.OnBehalfOfChain, "appended")
		*e.Envelope.RawOffset = 99
		return errors.New("discard partial reference changes")
	})
	bus := NewBus(BusOptions{Persister: p, Stages: []Stage{bad}, OnStageError: func(*StageError) {}})
	delegation := make([]string, 1, 4)
	delegation[0] = "msg://user/owner"
	offset := int64(7)
	e := Event{Scope: ScopeSession, Kind: "k", Envelope: Envelope{Actor: Actor{OnBehalfOfChain: delegation}, RawOffset: &offset}}
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := p.last().Envelope
	if len(got.Actor.OnBehalfOfChain) != 1 || got.Actor.OnBehalfOfChain[0] != "msg://user/owner" || got.RawOffset == nil || *got.RawOffset != 7 {
		t.Fatalf("failed stage leaked references: %+v", got)
	}
	if delegation[0] != "msg://user/owner" || delegation[:2][1] != "" || offset != 7 {
		t.Fatalf("failed stage mutated producer references: %v %d", delegation[:2], offset)
	}
}

type recObserver struct {
	seen  []Event
	after func(Event)
	boom  bool
}

func (o *recObserver) Observe(_ context.Context, e Event) {
	o.seen = append(o.seen, e)
	if o.boom {
		panic("observer bug")
	}
	if o.after != nil {
		o.after(e)
	}
}

func TestObserversSeeTheStoredEventAndMayPublishFollowUps(t *testing.T) {
	p := &envPersister{}
	obs := &recObserver{}
	var bus Bus
	obs.after = func(e Event) {
		if e.Kind == "first" {
			// Runs outside the bus lock, so this cannot deadlock.
			if err := bus.Publish(context.Background(), Event{Scope: ScopeSession, Kind: "follow-up"}); err != nil {
				t.Errorf("follow-up: %v", err)
			}
		}
	}
	bus = NewBus(BusOptions{Persister: p, Observers: []Observer{obs}})
	if err := bus.Publish(context.Background(), Event{Scope: ScopeSession, Kind: "first"}); err != nil {
		t.Fatal(err)
	}
	if len(obs.seen) != 2 || obs.seen[0].Kind != "first" || obs.seen[0].Seq != 1 || obs.seen[0].EventID != "evt_fake" || obs.seen[1].Kind != "follow-up" || obs.seen[1].Seq != 2 {
		t.Fatalf("observed %+v", obs.seen)
	}
}

func TestAPanickingObserverNeverLosesTheEventOrFailsThePublish(t *testing.T) {
	p := &envPersister{}
	bus := NewBus(BusOptions{Persister: p, Observers: []Observer{&recObserver{boom: true}}})
	if err := bus.Publish(context.Background(), Event{Scope: ScopeSession, Kind: "k"}); err != nil {
		t.Fatalf("publish failed because an observer did: %v", err)
	}
	if p.last().Kind != "k" {
		t.Fatal("event lost")
	}
}

func TestObserversAreNotCalledForARejectedPublish(t *testing.T) {
	obs := &recObserver{}
	bus := NewBus(BusOptions{Persister: &envPersister{}, Observers: []Observer{obs}})
	if err := bus.Publish(context.Background(), Event{Kind: "no scope"}); err == nil {
		t.Fatal("expected ErrScopeEmpty")
	}
	if len(obs.seen) != 0 {
		t.Fatalf("observed a rejected event: %+v", obs.seen)
	}
}
