//go:build !windows

package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/store"
)

func TestReplacementLineageUsesTxBusStages(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[reject], func(t *testing.T) {
			r, in, _ := replacementStoreRig(t)
			stage := eventstamp.New()
			stage.SetProcess(eventstamp.Process{EnvironmentID: "env"})
			bus := events.NewBus(events.BusOptions{Persister: r.svc.Store, Stages: []events.Stage{stage}}).(events.OutboxBus)
			called := 0
			r.svc.Store.SetEventTxPublisher(func(ctx context.Context, tx events.Tx, e events.Event) (events.Event, error) {
				called++
				stored, err := bus.PublishTx(ctx, tx, e)
				if err == nil && reject {
					return events.Event{}, errors.New("failure after audit insert")
				}
				return stored, err
			})
			_, err := prepareReplacement(t, r, in)
			if called != 1 || (err != nil) != reject {
				t.Fatal("lineage skipped bus or lost failure", called, err)
			}
			es, err := r.svc.Store.EventsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, e := range es {
				if e.Kind == "session.replaced_by" {
					count++
					if !strings.HasPrefix(e.EventID, "evt_") || e.Envelope.Dims.EnvironmentID != "env" || e.LogicalAgentID != in.Source.LogicalAgentID || e.Envelope.Dims.LogicalAgentID != in.Source.LogicalAgentID || e.Envelope.Actor.URN != "" {
						t.Fatalf("lineage metadata %+v", e)
					}
				}
			}
			if reject {
				if count != 0 {
					t.Fatal("failed transaction persisted lineage")
				}
				if _, err := r.svc.Store.GetSession(in.DestinationID); !errors.Is(err, store.ErrSessionNotFound) {
					t.Fatal("failed event committed replacement", err)
				}
			} else if count != 1 {
				t.Fatal("missing lineage", count)
			}
		})
	}
}
