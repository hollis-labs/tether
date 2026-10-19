package app

import (
	"context"
	"database/sql"
	"github.com/hollis-labs/tether/internal/launch"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/turnoutput"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

func TestTurnOutputReplayUsesSessionTaskNotNativeClaim(t *testing.T) {
	for _, task := range []string{"CW-20261012-0062", ""} {
		t.Run(task, func(t *testing.T) {
			svc, _ := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
			if _, err := svc.Store.DB().Exec(`UPDATE sessions SET task_id=? WHERE id='s1'`, sql.NullString{String: task, Valid: task != ""}); err != nil {
				t.Fatal(err)
			}
			job := &turnOutputWrite{row: store.SessionRow{ID: "s1", LogicalAgentID: "agent", ProjectID: "project"}, routeUnread: true, result: turnoutput.Output{TurnID: "turn", Kind: turnoutput.KindFinal, Text: "answer"}, origin: &outputEventOrigin{Envelope: events.Envelope{Dims: events.ScopeDims{TaskID: "forged", ProjectID: "forged", WorkstreamID: "forged"}}}}
			if err := job.journal(svc); err != nil {
				t.Fatal(err)
			}
			replay, err := readOutputRetry(svc, job.journalID)
			if err != nil {
				t.Fatal(err)
			}
			if err := replay.persist(context.Background(), svc); err != nil {
				t.Fatal(err)
			}
			payload := outputEvents(t, svc)[0]
			staged, err := svc.Store.StagedTurnOutput(context.Background(), payload.MessageID)
			if err != nil || payload.TaskID != task || staged.Metadata["task_id"] != task {
				t.Fatalf("correlation: payload=%+v metadata=%v err=%v", payload, staged.Metadata, err)
			}
			rows, err := svc.Store.EventsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range rows {
				if e.Kind == events.KindSessionTurnOutput {
					if d := e.Envelope.Dims; d.TaskID != task || d.ProjectID != "project" || d.WorkstreamID != "workstream" {
						t.Fatalf("scope=%+v", d)
					}
				}
			}
		})
	}
}

func TestWorkerLaunchSealsSessionCorrelation(t *testing.T) {
	r := newIdentityRig(t)
	for _, task := range []string{"CW-20261012-0062", ""} {
		id := "correlation-" + task
		plan := &launch.Plan{ProviderBrand: "claude", Env: map[string]string{"TETHER_TASK_ID": "forged", "TETHER_PROJECT_ID": "forged", "TETHER_WORKSTREAM_ID": "forged", "KEEP": "yes"}}
		row := store.SessionRow{ID: id, WorkerID: r.worker, ProjectID: "PRJ-1", TaskID: sql.NullString{String: task, Valid: task != ""}, WorkstreamID: sql.NullString{String: "WS-1", Valid: true}, State: "created", Workspace: t.TempDir()}
		if err := r.db.CreateSession(row, plan); err != nil {
			t.Fatal(err)
		}
		if _, err := r.svc.launchWorkerSession(context.Background(), &row, plan); err != nil {
			t.Fatal(err)
		}
		select {
		case spec := <-r.specs:
			got := launch.CorrelationFromEnv(spec.Env)
			if got != (launch.Correlation{TaskID: task, ProjectID: "PRJ-1", WorkstreamID: "WS-1"}) || spec.Env["KEEP"] != "yes" {
				t.Fatalf("incorrect worker correlation: %+v", got)
			}
			if plan.Env["TETHER_TASK_ID"] != "forged" {
				t.Fatal("launch changed persisted-plan input")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("worker received no spec")
		}
	}
}
