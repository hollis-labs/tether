package app

import (
	"context"
	"encoding/json"
	gomsg "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"path/filepath"
	"testing"
	"time"

	gopevents "github.com/hollis-labs/substrate/harness/adapters/provider/events"
	"github.com/hollis-labs/substrate/harness/adapters/turnoutput"
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

func TestServiceCloseJoinsBlockedRouterBeforeClosingStore(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	if err := svc.Store.CreateSession(store.SessionRow{ID: "blocked", State: "running"}, &launch.Plan{Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.StageTurnOutput(context.Background(), gomsg.Envelope{From: gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: "blocked"}, Payload: []byte(`{"text":"answer"}`), ContentType: "application/json", Metadata: map[string]string{"session_id": "blocked", "kind": "final"}}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	checked := make(chan error, 1)
	svc.Store.SetChannelAuthorization(func(ctx context.Context, _ string, _ string, _ identity.Principal, _ gomsg.Address) error {
		close(entered)
		<-ctx.Done()
		checked <- svc.Store.DB().PingContext(context.Background())
		return ctx.Err()
	})
	if err := svc.startTurnRouter(); err != nil {
		t.Fatal(err)
	}
	defer svc.turnRouter.Close()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("router never entered hook")
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-checked; err != nil {
		t.Fatal("store closed before router joined", err)
	}
}
