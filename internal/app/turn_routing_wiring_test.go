package app

import (
	"context"
	"slices"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestInstalledRoutingFeedKindsAndWorkerLifecycle(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	svc.Catalog = &config.Catalog{Providers: map[string]config.Provider{
		"claude":   {ID: "claude", Provider: "claude", RuntimeKind: config.RuntimeKindStreamingStdio},
		"codex":    {ID: "codex", Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio},
		"agy":      {ID: "agy", Provider: "agy", RuntimeKind: config.RuntimeKindSubprocess},
		"opencode": {ID: "opencode", Provider: "opencode", RuntimeKind: config.RuntimeKindSubprocess},
	}}
	svc.installTurnFeeds()
	kinds, running := svc.RoutingWiring()
	if running || !slices.Equal(kinds, []string{"approval", "failure", "final", "question"}) {
		t.Fatalf("before daemon: %v %v", kinds, running)
	}
	if slices.Contains(svc.RoutingRuntimeKinds("antigravity"), "question") || !slices.Contains(svc.RoutingRuntimeKinds("antigravity"), "approval") {
		t.Fatal("antigravity detector claims")
	}
	if len(svc.RoutingRuntimeKinds("agy")) != 0 || len(svc.RoutingRuntimeKinds("missing")) != 0 || slices.Contains(svc.RoutingRuntimeKinds("opencode"), "approval") {
		t.Fatal("unregistered detector advertised")
	}
	if err := svc.startTurnRouter(); err != nil {
		t.Fatal(err)
	}
	_, running = svc.RoutingWiring()
	if !running {
		t.Fatal("worker readiness absent")
	}
	capabilities, err := svc.RoutingCapabilities(context.Background(), "")
	if err != nil || !capabilities.RouteSupported || !slices.Contains(capabilities.Runtimes["codex"].KindsAvailable, "question") {
		t.Fatalf("capabilities: %+v %v", capabilities, err)
	}
	svc.turnRouter.Close()
	_, running = svc.RoutingWiring()
	if running {
		t.Fatal("closed worker advertised")
	}
}

func TestInstalledTypedSignalsOnlyClassifyEndedSignal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []gopevents.Event
		want   turnoutput.Kind
	}{
		{"question", []gopevents.Event{gopevents.ToolUse{Name: "request_user_input", ID: "question", Args: map[string]any{"question": "Where?"}}, gopevents.Done{}}, turnoutput.KindQuestion},
		{"approval", []gopevents.Event{gopevents.PermissionDenied{Action: "Bash", DisplayName: "make deploy", ToolUseID: "denied"}, gopevents.Done{}}, turnoutput.KindApproval},
		{"continued after question", []gopevents.Event{gopevents.ToolUse{Name: "request_user_input", ID: "question"}, gopevents.ToolResult{ID: "question", ContentPreview: "here"}, gopevents.Done{Text: "Finished."}}, turnoutput.KindFinal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := outputHarness(t, nil)
			svc.Catalog = &config.Catalog{Providers: map[string]config.Provider{"codex": {ID: "codex", Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio}}}
			svc.installTurnFeeds()
			row, err := svc.Store.GetSession("s1")
			if err != nil {
				t.Fatal(err)
			}
			output := svc.newSessionTurnOutput(*row, &launch.Plan{ProviderBrand: "codex"})
			for _, ev := range tc.events {
				svc.turnFeeds["codex"].observe(output, ev)
			}
			got := outputEvents(t, svc)
			if len(got) != 1 || got[0].Kind != tc.want {
				t.Fatalf("outputs: %+v", got)
			}
		})
	}
}
