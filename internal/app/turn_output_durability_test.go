package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	"github.com/hollis-labs/go-providers/provider"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

func waitOutputEvents(t *testing.T, svc *Service, n int) []events.TurnOutputEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := outputEvents(t, svc)
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("retry did not publish output")
	return nil
}

func TestTurnOutputJournalRecoversBeforeMetadataOrStageAfterRestart(t *testing.T) {
	svc, _ := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	svc.turnOutputTimeout = 20 * time.Millisecond
	var index int
	var schema, statePath string
	if err := svc.Store.DB().QueryRow(`PRAGMA database_list`).Scan(&index, &schema, &statePath); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	job := &turnOutputWrite{row: *row, route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}},
		result: turnoutput.Output{TurnID: "lost-before-stage", Kind: turnoutput.KindFinal, Text: strings.Repeat("complete answer", 1000)}}
	conn, err := svc.Store.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := svc.outputPersistenceContext()
	err = job.persist(ctx, svc)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked DB: %v", err)
	}
	ids, err := svc.Store.PendingTurnOutputRetries("", 128)
	if err != nil || len(ids) != 1 {
		t.Fatal("no durable journal before first DB read", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate losing all in-memory jobs and opening a new daemon composition.
	if err := svc.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replacement := &Service{Store: reopened, Bus: events.NewBus(events.BusOptions{Persister: reopened})}
	if err := replacement.startTurnRouter(); err != nil {
		t.Fatal(err)
	}
	defer replacement.turnRouter.Close()
	defer replacement.stopOutputRetries()
	got := waitOutputEvents(t, replacement, 1)
	if got[0].OutputID != job.journalID || got[0].MessageID == "" {
		t.Fatal("restart did not recover staged attribution")
	}
	var stagedPayload json.RawMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		history, err := replacement.Store.ReadChannel(context.Background(), "ops", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(history) == 1 && history[0].ID == got[0].MessageID {
			stagedPayload = history[0].Payload
			if history[0].Metadata["output_id"] != job.journalID {
				t.Fatal("routed consumer lost output identity")
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stagedPayload == nil {
		t.Fatal("restart did not route recovered body")
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(stagedPayload, &body); err != nil || body.Text != job.result.Text {
		t.Fatal("restart lost full selected body", err)
	}
}

func TestTurnOutputJournalAcknowledgesAlreadyPublishedOutputWithoutRepublishing(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	job := &turnOutputWrite{row: *row, result: turnoutput.Output{TurnID: "ack-window", Kind: turnoutput.KindFinal, Text: "answer"}}
	if err := job.journal(svc); err != nil {
		t.Fatal(err)
	}
	data, err := svc.Store.ReadTurnOutputRetry(job.journalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := job.persist(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	// Crash immediately after Publish but before acknowledgement: replay that
	// same committed journal record against the already-persisted event history.
	if err := svc.Store.WriteTurnOutputRetry(job.journalID, data); err != nil {
		t.Fatal(err)
	}
	replay, err := readOutputRetry(svc, job.journalID)
	if err != nil {
		t.Fatal(err)
	}
	if err := replay.persist(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if got := outputEvents(t, svc); len(got) != 1 {
		t.Fatal("publication replay duplicated output")
	}
	if ids, err := svc.Store.PendingTurnOutputRetries("", 128); err != nil || len(ids) != 0 {
		t.Fatal("published journal not acknowledged", err)
	}
}

func TestTurnOutputJournalPreservesUnroutedExcerptPolicy(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	job := &turnOutputWrite{row: *row, result: turnoutput.Output{TurnID: "excerpt", Kind: turnoutput.KindFinal, Text: strings.Repeat("x", 6000)}}
	if err := job.journal(svc); err != nil {
		t.Fatal(err)
	}
	replay, err := readOutputRetry(svc, job.journalID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.result.Text) != 4096 || !replay.textTruncated {
		t.Fatal("journal archived full unrouted text")
	}
	if err := replay.persist(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	got := outputEvents(t, svc)[0]
	if len(got.Text) != 4096 || !got.TextTruncated || got.MessageID != "" {
		t.Fatal("replay changed unrouted publication policy")
	}
}

func TestTurnOutputFreshConversationAfterLostSubmission(t *testing.T) {
	svc, output := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	svc.turnOutputs.Store("s1", output)
	if err := svc.trackTurnSubmission("s1", func() error { return provider.ErrProviderSessionLost }); !errors.Is(err, provider.ErrProviderSessionLost) {
		t.Fatal(err)
	}
	output.observeProvider(gopevents.Done{Text: "fresh"})
	output.observeProvider(gopevents.Done{Text: "continued"})
	got := outputEvents(t, svc)
	if len(got) != 2 || !got[0].FreshConversation || got[1].FreshConversation {
		t.Fatal("session loss marker not confined to next turn")
	}
	env, err := svc.Store.StagedTurnOutput(context.Background(), got[0].MessageID)
	if err != nil || env.Metadata["fresh_conversation"] != "true" {
		t.Fatal("routed consumer lost continuity marker", err)
	}
}

func TestTurnOutputTypedSessionLossMarksFreshConversation(t *testing.T) {
	for _, feed := range []string{"provider", "runtime", "recovery"} {
		t.Run(feed, func(t *testing.T) {
			svc, output := outputHarness(t, nil)
			svc.turnOutputs.Store("s1", output)
			switch feed {
			case "provider":
				output.observeProvider(gopevents.SessionLost{Reason: "fixture"})
			case "runtime":
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindSessionLost})
			case "recovery":
				svc.MarkTurnOutputSessionLost("s1")
			}
			output.observeProvider(gopevents.Done{Text: "fresh"})
			if got := outputEvents(t, svc); len(got) != 1 || !got[0].FreshConversation {
				t.Fatal("typed loss did not mark next output")
			}
		})
	}
}

func TestTurnOutputTrackerCreatesPendingLogWithoutInventingOutput(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	row.Workspace = workspace
	svc.newSessionTurnOutput(*row, &launch.Plan{ProviderBrand: "codex"})
	info, err := os.Stat(filepath.Join(workspace, "logs", "session.log"))
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatal("pending log absent or populated without output", err)
	}
	if got := outputEvents(t, svc); len(got) != 0 {
		t.Fatal("empty log creation invented completion")
	}
}
