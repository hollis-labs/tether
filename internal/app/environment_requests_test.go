package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestEnvironmentRequestPolicyAndMetadata(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "suppressed", true: "accepted"}[supported], func(t *testing.T) {
			svc, _ := outputHarness(t, nil)
			brand := "missing"
			if supported {
				brand = "codex"
				svc.Catalog = &config.Catalog{Providers: map[string]config.Provider{"codex": {ID: "codex", Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio}}}
				svc.installTurnFeeds()
			}
			row, err := svc.Store.GetSession("s1")
			if err != nil {
				t.Fatal(err)
			}
			output := svc.newSessionTurnOutput(*row, &launch.Plan{ProviderBrand: brand})
			payload := json.RawMessage(`{"request_id":17,"method":"item/tool/requestUserInput","params":{"secret":"not-for-event-log"}}`)
			output.observeRuntime(runtimeevents.Event{ID: "request-event", Kind: runtimeevents.KindAgentPermissionRequested, TurnID: "t", Sequence: 1, Payload: payload})
			base, err := svc.Store.EnvironmentSnapshot(context.Background(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			if !supported && len(base.Requests) != 0 {
				t.Fatal("suppressed signal established known state")
			}
			if supported {
				if len(base.Requests) != 1 || !base.Requests[0].Open || base.Requests[0].Kind != "question" {
					t.Fatalf("projection %+v", base)
				}
				events, err := svc.Store.EventsSince(0)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Kind == store.EnvironmentRequestKind && strings.Contains(event.PayloadJSON, "not-for-event-log") {
						t.Fatal("request contents leaked into event")
					}
				}
				// Disconnecting the control plane is not a terminal turn.
				if err := svc.Store.UpdateSessionState("s1", "orphaned", 0, nil); err != nil {
					t.Fatal(err)
				}
				orphan, err := svc.Store.EnvironmentSnapshot(context.Background(), "s1")
				if err != nil || !orphan.Requests[0].Open {
					t.Fatal("orphaning cleared request", err)
				}
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "other", Sequence: 2})
				other, err := svc.Store.EnvironmentSnapshot(context.Background(), "s1")
				if err != nil || !other.Requests[0].Open {
					t.Fatal("other terminal turn cleared request", err)
				}
				output.observeRuntime(runtimeevents.Event{Kind: runtimeevents.KindTurnCompleted, TurnID: "t", Sequence: 3})
				ended, err := svc.Store.EnvironmentSnapshot(context.Background(), "s1")
				if err != nil || ended.Requests[0].Open {
					t.Fatal("exact terminal turn did not close request", err)
				}
			}
		})
	}
}

func TestEnvironmentRequestIdentityRejectsObjects(t *testing.T) {
	if got := scalarRequestID(json.RawMessage(`{"params":"must-not-be-an-identity"}`)); got != "" {
		t.Fatal("object became identity", got)
	}
	if got := scalarRequestID(json.RawMessage(`"request-id"`)); got != `request:"request-id"` {
		t.Fatal(got)
	}
	if got := scalarRequestID(json.RawMessage(`17`)); got != "request:17" {
		t.Fatal(got)
	}
}
