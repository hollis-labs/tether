package app

import (
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"testing"
)

func TestRoutingInterruptRequiresDescriptorAndActualWiring(t *testing.T) {
	svc := routingTestService()
	svc.Catalog.Providers["claude"] = config.Provider{ID: "claude", Provider: "claude", RuntimeKind: config.RuntimeKindStreamingStdio}
	svc.Manager = agentsessions.NewManager(nil)
	svc.Store = &store.Store{}
	factory := func(*launch.Plan) (agentsessions.Runtime, error) { panic("capability inspection launched a runtime") }
	svc.factories = map[string]RuntimeFactory{"codex": factory, "claude": factory, "agy": factory}
	for _, missing := range []string{"", "manager", "store", "factory"} {
		t.Run(missing, func(t *testing.T) {
			candidate := &Service{Catalog: svc.Catalog, Manager: svc.Manager, Store: svc.Store, factories: svc.factories}
			switch missing {
			case "manager":
				candidate.Manager = nil
			case "store":
				candidate.Store = nil
			case "factory":
				candidate.factories = nil
			}
			got, err := candidate.RoutingCapabilities(interruptTestContext(t), "")
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"codex", "claude"} {
				if got.Runtimes[id].Interrupt != (missing == "") {
					t.Fatalf("%s %s capability=%+v", missing, id, got)
				}
			}
			if got.Runtimes["antigravity"].Interrupt {
				t.Fatal("unsupported descriptor invented cancellation")
			}
		})
	}
}
