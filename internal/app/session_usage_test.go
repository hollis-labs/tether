package app

import (
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestMeterTurnRecordsUsageAndMarksUnmetered(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	output := svc.newSessionTurnOutput(*row, &launch.Plan{ProviderBrand: "missing"})
	t0 := time.Now()
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnStarted, TurnID: "t1", Time: t0})
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "t1", Time: t0.Add(2 * time.Second),
		Payload: []byte(`{"usage":{"InputTokens":100,"OutputTokens":40,"CostUSD":0.5}}`)})
	// A turn with no usage object counts as a turn but adds no tokens.
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnFailed, TurnID: "t2", Time: t0.Add(3 * time.Second)})
	u, err := svc.Store.GetSessionUsage("s1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Turns != 2 || u.FailedTurns != 1 || u.UsageTurns != 1 || u.InputTokens != 100 || u.OutputTokens != 40 || u.CostUSD != 0.5 || u.TurnMS != 2000 {
		t.Fatalf("usage %+v", u)
	}
}
