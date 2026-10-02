package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestServiceCloseFlushesOutputAndJoinsRouterBeforeStorageClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	row := store.SessionRow{ID: "closing", State: "running"}
	if err := db.CreateSession(row, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Bus: events.NewBus(events.BusOptions{Persister: db})}
	output := svc.newSessionTurnOutput(row, &launch.Plan{ProviderBrand: "codex"})
	svc.turnOutputs.Store(row.ID, output)
	output.observeProvider(gopevents.Delta{Text: "partial text at shutdown"})
	marker, done := output.CurrentTurn()
	if err := svc.startTurnRouter(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if svc.turnRouter.Running() {
		t.Fatal("router still running against closed storage")
	}
	select {
	case <-done:
	default:
		t.Fatal("output was not completed before close")
	}
	if completed, ok := output.CompletedTurn(marker); !ok || completed != marker {
		t.Fatal("shutdown did not record bound completion")
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	evs, err := reopened.QueryEvents(store.EventFilter{Kinds: []string{events.KindSessionTurnOutput}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("lost output at shutdown: %+v %v", evs, err)
	}
	var payload events.TurnOutputEvent
	if err := json.Unmarshal([]byte(evs[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != turnoutput.KindTerminal || payload.Text != "partial text at shutdown" {
		t.Fatalf("shutdown output: %+v", payload)
	}
	if _, err := reopened.SessionRoute(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
}
