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
			}
		})
	}
}
