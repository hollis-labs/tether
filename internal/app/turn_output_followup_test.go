package app

import (
	"context"
	"errors"
	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"testing"
	"time"

	gopevents "github.com/hollis-labs/go-providers/provider/events"
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
	return s.Store.GetSessionContext(ctx, id)
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
