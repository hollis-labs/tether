package app

// interrupt:true on a reply, against REAL agentkit sessions and the real
// Service.CancelTurnAndWait (CW-20261002-0067); only the model CLI is a captured
// or scripted fixture.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	llmtypes "github.com/hollis-labs/go-llm-types"
	messaging "github.com/hollis-labs/go-messaging"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/providertest"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
	"github.com/hollis-labs/tether/internal/store"
)

func publishRoutedFrom(t *testing.T, svc *Service, session string) messaging.Envelope {
	t.Helper()
	channel, err := channels.ChannelAddress("ops")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := svc.Store.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: session}, To: channel,
		Payload: []byte(`{"text":"which option?"}`), ContentType: "application/json", Metadata: map[string]string{"session_id": session}})
	if err != nil {
		t.Fatal(err)
	}
	return parent
}

// A reply with interrupt:true cancels the Codex app-server turn that is running,
// waits for it to end, and is then the session's next turn on the SAME process.
func TestReplyInterruptsARealCodexTurnThenBecomesTheNextTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtimes.Codex, providertest.Replay("codex/app_server_interrupt"))
	rt, err := claudestream.NewWithAdapter(&launch.Plan{Command: fake.Path}, gop.NewCodexAdapterAppServer(), "codex",
		agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}, claudestream.WithoutPreflight())
	if err != nil {
		t.Fatal(err)
	}
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	svc.Manager = agentsessions.NewManager(nil)
	working, nextDone := make(chan struct{}), make(chan struct{})
	var workingOnce, doneOnce sync.Once
	dir := t.TempDir()
	opts := agentsessions.StartOptions{Workdir: dir, LogPath: filepath.Join(dir, "session.log"), JsonRpcNotificationHook: func(method string, params json.RawMessage) {
		if method == "item/started" {
			workingOnce.Do(func() { close(working) })
		}
		if method == "turn/completed" {
			var p struct {
				Turn struct {
					Status string `json:"status"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(params, &p)
			if p.Turn.Status == "completed" {
				doneOnce.Do(func() { close(nextDone) })
			}
		}
	}}
	output.wire(rt, &opts)
	if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: "s1", Runtime: rt, Options: opts}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.stopRoutingReplies()
		_ = svc.Manager.Stop(context.Background(), "s1")
		_ = svc.Manager.Shutdown(context.Background())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := svc.StartRoutingReplies(ctx); err != nil {
		t.Fatal(err)
	}
	parent := publishRoutedFrom(t, svc, "s1")

	if err := svc.SendTurn(ctx, "s1", "first turn"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	interrupted, _ := output.CurrentTurn()

	receipt, err := svc.SubmitRoutingReply(ctx, api.RoutingReplyRequest{ParentID: parent.ID, Body: "stop, do this instead", Interrupt: true,
		Verified: true, Caller: identity.Principal{ID: "msg://user/local/alice", Kind: "user"}})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Interrupt != llmtypes.StopReasonCancelled || receipt.State != "queued" || receipt.TargetSessionID != "s1" {
		t.Fatalf("receipt = %+v", receipt)
	}
	select {
	case <-nextDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var delivered store.RoutingReply
	for ctx.Err() == nil {
		if delivered, _ = svc.Store.RoutingReply(ctx, receipt.ReplyID); delivered.State == store.RoutingReplyDelivered {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if delivered.State != store.RoutingReplyDelivered || delivered.DeliveredToSessionID != "s1" {
		t.Fatalf("reply = %+v", delivered)
	}
	outputs := outputEvents(t, svc)
	if len(outputs) != 2 || outputs[0].TurnID != interrupted || outputs[1].TurnID == interrupted {
		t.Fatalf("completed turns = %+v (the interrupted turn, then the reply's)", outputs)
	}
	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("the interrupt restarted the Codex process (%d invocations)", n)
	}
}

// A subprocess runtime cannot cancel a turn: the reply is refused with the typed
// interrupt_unsupported error, nothing is queued, and the turn keeps running.
func TestReplyInterruptIsRefusedOnARealSubprocessRuntime(t *testing.T) {
	var steps []providertest.Step
	for _, line := range providertest.FixtureLines(t, "antigravity/print_turn1.jsonl")[:4] {
		steps = append(steps, providertest.Stdout(string(line)))
	}
	steps = append(steps, providertest.Hang()) // the turn is open until the session is stopped
	svc, id := nativeOutputLaunch(t, runtimes.Antigravity, "subprocess", providertest.Script(steps...))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := svc.StartRoutingReplies(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.stopRoutingReplies)
	parent := publishRoutedFrom(t, svc, id)

	sent := make(chan error, 1)
	go func() { sent <- svc.SendTurn(ctx, id, "work forever") }()
	state, ok := svc.SessionTurnOutputState(id)
	if !ok {
		t.Fatal("no turn state")
	}
	for { // wait until the runtime has started the turn
		if turn, _ := state.CurrentTurn(); turn != "" && state.TurnAccepted(turn) {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the scripted turn never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err := svc.SubmitRoutingReply(ctx, api.RoutingReplyRequest{ParentID: parent.ID, Body: "stop", Interrupt: true, Verified: true,
		Caller: identity.Principal{ID: "msg://user/local/alice", Kind: "user"}})
	if !errors.Is(err, api.ErrReplyInterruptUnsupported) {
		t.Fatalf("got %v, want the typed interrupt_unsupported refusal", err)
	}
	if rows, _ := svc.Store.RoutingRepliesInState(ctx, store.RoutingReplyQueued, 10); len(rows) != 0 {
		t.Fatalf("a refused interrupt must not queue the reply: %+v", rows)
	}
	if turn, _ := state.CurrentTurn(); turn == "" {
		t.Fatal("the refused interrupt ended the running turn")
	}
	_ = svc.Manager.Stop(context.Background(), id) // end the hung process
	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("SendTurn did not return after the session was stopped")
	}
}
