package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	svc, id := replyLaunchCommand(t, config.Provider{ID: string(runtime), Type: "cli", Command: fake.Path, RuntimeKind: mode}, fake.Path, nil)
	return svc, id, fake
}

// replyLaunchCommand launches a real session of prov running command under a
// Service whose router publishes to channel "ops".
func replyLaunchCommand(t *testing.T, prov config.Provider, command string, args []string) (*Service, string) {
	t.Helper()
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
		WorkspaceMode: "shared", Command: command, Args: args, BootMode: "none", Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}
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
	return svc, id
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

// A CLI that is launched and then refuses the turn for want of a login exits
// non-zero, and Tether's turn feed still shows a turn (a synthesized terminal is
// emitted on every subprocess exit). The model never saw the reply, so it must be
// retried and end undeliverable, never settled as delivered with a failed turn.
func TestACLIThatRefusesTheTurnForWantOfALoginIsRetriedNotMarkedDelivered(t *testing.T) {
	oldBackoff := replySubmitBackoff
	replySubmitBackoff = 5 * time.Millisecond
	t.Cleanup(func() { replySubmitBackoff = oldBackoff }) // registered first, so it runs after the dispatcher stops
	svc, id, fake := replyLaunch(t, runtimes.Antigravity, "subprocess",
		providertest.Script(providertest.Stderr("not authenticated: no stored credentials found"), providertest.Exit(1)).Always())
	if err := svc.StartRoutingReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	parent := publishRoutedFrom(t, svc, id)
	receipt, err := replyAs(svc, parent.ID, "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	got := waitTerminal(t, svc, receipt.ReplyID)
	if got.State != store.RoutingReplyUndeliverable || got.Reason != ReplyReasonSubmitFailed || got.Attempts != replyMaxAttempts {
		t.Fatalf("reply = %+v, want undeliverable/%s after %d attempts", got, ReplyReasonSubmitFailed, replyMaxAttempts)
	}
	if n := len(fake.Calls()); n != replyMaxAttempts {
		t.Fatalf("the CLI was invoked %d times, want %d", n, replyMaxAttempts)
	}
}

// The no-turn-feed refusal reads the process layer: a running PTY has no turn
// lifecycle, so nothing says when it is idle. The unit tests inject the flag; this
// runs the production detector against real sessions.
func TestTheProductionDetectorRefusesAReplyToARunningPTYAndOnlyThat(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	t.Setenv("HOME", t.TempDir())
	// The PTY adapter appends its own flags, so the command must ignore its arguments
	// and stay alive for as long as the session does.
	idle := filepath.Join(t.TempDir(), "idle-cli")
	if err := os.WriteFile(idle, []byte("#!"+sh+"\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pty, ptyID := replyLaunchCommand(t, config.Provider{ID: "claude", Provider: "claude", Type: "cli", Command: idle, RuntimeKind: config.RuntimeKindPTY}, idle, nil)
	if !pty.replyNoTurnFeed(ptyID) {
		t.Fatal("a running PTY session is reported as having a turn feed")
	}
	if err := pty.StartRoutingReplies(context.Background()); err != nil {
		t.Fatal(err)
	}
	parent := publishRoutedFrom(t, pty, ptyID)
	if _, err := replyAs(pty, parent.ID, "hello?"); !errors.Is(err, api.ErrReplyNoTurnFeed) {
		t.Fatalf("a reply to a running PTY: %v, want the typed no-turn-feed refusal", err)
	}

	sub, subID, _ := replyLaunch(t, runtimes.Codex, "subprocess", providertest.Script(providertest.Exit(0)).Always())
	if sub.replyNoTurnFeed(subID) {
		t.Fatal("a subprocess session is reported as having no turn feed")
	}
	if sub.replyNoTurnFeed("no-such-session") {
		t.Fatal("an unknown session is reported as a PTY")
	}
}
