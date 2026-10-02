package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

type routingTestWiring struct {
	kinds                    []string
	router, reply, interrupt bool
}

func (w routingTestWiring) RoutingWiring() ([]string, bool)   { return w.kinds, w.router }
func (w routingTestWiring) RoutingReplyWired() bool           { return w.reply }
func (w routingTestWiring) RoutingInterruptWired(string) bool { return w.interrupt }

type routingRuntimeTestWiring struct{ routingTestWiring }

func (w routingRuntimeTestWiring) RoutingRuntimeKinds(id string) []string {
	if id == "codex" {
		return []string{"final", "failure", "question", "approval"}
	}
	return []string{"final", "failure"}
}

func TestRoutingKindsRequireRuntimeDetectors(t *testing.T) {
	svc := routingTestService()
	base := routingTestWiring{kinds: []string{"final", "failure", "question", "approval", "terminal"}, router: true}
	got, err := svc.routingCapabilities(context.Background(), "", base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.KindsAvailable, []string{"failure", "final"}) {
		t.Fatalf("absent detector hook = %+v", got)
	}
	got, err = svc.routingCapabilities(context.Background(), "", routingRuntimeTestWiring{base})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Runtimes["codex"].KindsAvailable, []string{"final", "failure", "question", "approval"}) {
		t.Fatalf("wired codex = %+v", got)
	}
	if !reflect.DeepEqual(got.Runtimes["antigravity"].KindsAvailable, []string{"final", "failure"}) {
		t.Fatalf("unwired antigravity detectors = %+v", got)
	}
}

func routingTestService() *Service {
	return &Service{Catalog: &config.Catalog{Providers: map[string]config.Provider{
		"agy":   {ID: "agy", Provider: "agy", RuntimeKind: config.RuntimeKindSubprocess},
		"codex": {ID: "codex", Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio},
	}}}
}

func TestRoutingCapabilitiesRequireInstalledPaths(t *testing.T) {
	svc := routingTestService()
	ctx := context.Background()
	initial, err := svc.RoutingCapabilities(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if initial.RouteSupported || initial.ReplyToSender || initial.Interrupt || len(initial.KindsAvailable) != 0 || initial.Delivery != "next-turn" {
		t.Fatalf("unwired = %+v", initial)
	}
	if _, ok := initial.Runtimes["agy"]; ok {
		t.Fatal("alias leaked as registry id")
	}
	if initial.Runtimes["antigravity"].FinalTextConfidence != "unknown" {
		t.Fatal("unwired publisher advertised confidence")
	}
	wiring := routingTestWiring{kinds: []string{"final", "failure"}}
	published, err := svc.routingCapabilities(ctx, "", wiring)
	if err != nil {
		t.Fatal(err)
	}
	if published.RouteSupported || published.Runtimes["codex"].FinalTextConfidence != "exact" {
		t.Fatalf("publisher without router = %+v", published)
	}
	wiring.router, wiring.reply, wiring.interrupt = true, true, true
	installed, err := svc.routingCapabilities(ctx, "", wiring)
	if err != nil {
		t.Fatal(err)
	}
	if !installed.RouteSupported || !installed.ReplyToSender || !reflect.DeepEqual(installed.KindsAvailable, []string{"failure", "final"}) {
		t.Fatalf("wired = %+v", installed)
	}
	// A service hook alone cannot invent a cancel_turn advertisement hidden
	// by today's PlanScopedAdapter. Per-turn Antigravity also cannot cancel.
	if installed.Runtimes["antigravity"].Interrupt {
		t.Fatal("per-turn runtime advertised interrupt")
	}
}

func TestRoutingSessionUsesPersistedMode(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "routing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := routingTestService()
	svc.Store = db
	plan := &launch.Plan{ProviderID: "codex", ProviderBrand: "claude", RuntimeKind: config.RuntimeKindPTY}
	if err := db.CreateSession(store.SessionRow{ID: "persisted", ProviderID: "codex"}, plan); err != nil {
		t.Fatal(err)
	}
	got, err := svc.routingCapabilities(context.Background(), "persisted", routingTestWiring{kinds: []string{"final"}, router: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "persisted" || got.RouteSupported || got.Runtimes["claude"].FinalTextConfidence != "unknown" {
		t.Fatalf("persisted mode = %+v", got)
	}
	if _, ok := got.Runtimes["codex"]; ok {
		t.Fatal("current catalog replaced persisted launch")
	}
	_, err = svc.RoutingCapabilities(context.Background(), "missing")
	if !errors.Is(err, ErrRoutingSessionNotFound) {
		t.Fatalf("missing = %v", err)
	}
}
