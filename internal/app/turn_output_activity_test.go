package app

import (
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
	"testing"
	"time"
)

func TestSessionLastActivityTracksStreamButNotTelemetry(t *testing.T) {
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	if !svc.SessionLastActivity("missing").IsZero() {
		t.Fatal("invented missing-session activity")
	}
	output.observeProvider(gopevents.Heartbeat{})
	output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindSessionLost})
	if !svc.SessionLastActivity("s1").IsZero() {
		t.Fatal("telemetry counted as turn activity")
	}
	output.observeProvider(gopevents.Delta{Text: "first"})
	first := svc.SessionLastActivity("s1")
	if first.IsZero() {
		t.Fatal("stream activity not observed")
	}
	time.Sleep(time.Millisecond)
	output.observeProvider(gopevents.Delta{Text: "second"})
	if !svc.SessionLastActivity("s1").After(first) {
		t.Fatal("mid-turn stream activity not refreshed")
	}
}
