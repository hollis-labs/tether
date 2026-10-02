package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/providertest"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

// replyLaunch is nativeOutputLaunch that also hands back the fake CLI, so a test
// can count how many times the model process was actually invoked.
func replyLaunch(t *testing.T, runtime runtimes.ID, mode string, run providertest.Run) (*Service, string, *providertest.Fake) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtime, run)
	prov := config.Provider{ID: string(runtime), Type: "cli", Command: fake.Path, RuntimeKind: mode}
	factory, err := runtimeFactoryForProvider(prov)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "reply.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id := "reply-session"
	plan := &launch.Plan{LaunchID: "launch", ProjectID: "project", LogicalAgentID: "agent", ProviderID: prov.ID,
		ProviderBrand: prov.ProviderBrand(), RuntimeKind: prov.EffectiveRuntimeKind(), RepoRoot: t.TempDir(), WriteHome: t.TempDir(),
		WorkspaceMode: "shared", Command: fake.Path, BootMode: "none", Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}
	ws, err := workspace.Create(plan.WriteHome, id, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: id, LogicalAgentID: "agent", ProjectID: "project", Workspace: ws.Root, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Bus: events.NewBus(events.BusOptions{Persister: db}), CatalogRoot: t.TempDir(),
		Catalog: &config.Catalog{Global: config.Global{Version: "test"}}, Manager: agentsessions.NewManager(stateSinkAdapter{db: db}), factories: map[string]RuntimeFactory{prov.ID: factory}}
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.stopRoutingReplies(); _ = svc.Manager.Stop(context.Background(), id) })
	return svc, id, fake
}

func replyAs(svc *Service, parentID, body string) (api.RoutingReplyReceipt, error) {
	return svc.SubmitRoutingReply(context.Background(), api.RoutingReplyRequest{ParentID: parentID, Body: body, Verified: true,
		Caller: identity.Principal{ID: "msg://user/local/chris", Kind: "user"}})
}

func waitTerminal(t *testing.T, svc *Service, id string) store.RoutingReply {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, err := svc.Store.RoutingReply(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State.Terminal() {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := svc.Store.RoutingReply(context.Background(), id)
	t.Fatalf("reply never settled: %+v", r)
	return r
}

// A subprocess runtime's SendTurn blocks for the whole turn and returns the
// failure AFTER the model ran the reply. Retrying would run the reply again: the
// turn must be reported, not repeated.
func TestAFailedSubprocessTurnIsNotRetried(t *testing.T) {
	svc, id, fake := replyLaunch(t, runtimes.Codex, "subprocess", providertest.Script(providertest.Stderr("fixture process failure"), providertest.Exit(1)))
	ctx := context.Background()
	if err := svc.StartRoutingReplies(ctx); err != nil {
		t.Fatal(err)
	}
	parent := publishRoutedFrom(t, svc, id)
	receipt, err := replyAs(svc, parent.ID, "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	got := waitTerminal(t, svc, receipt.ReplyID)
	time.Sleep(300 * time.Millisecond) // a retry, if any, would have started by now
	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("the model process was invoked %d times for one reply (%+v)", n, got)
	}
	if got.State != store.RoutingReplyDelivered || got.Reason != ReplyReasonTurnFailed || got.Attempts != 1 {
		t.Fatalf("reply = %+v, want delivered with reason %s after one attempt", got, ReplyReasonTurnFailed)
	}
}
