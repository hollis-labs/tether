package app

import (
	"context"
	"errors"
	"testing"

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
