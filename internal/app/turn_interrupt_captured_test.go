package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-providers/providertest"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/acp"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
)

func TestCancelTurnAndWaitCapturedCodexThenNextTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtimes.Codex, providertest.Replay("codex/app_server_interrupt"))
	rt, err := claudestream.NewWithAdapter(&launch.Plan{Command: fake.Path}, gop.NewCodexAdapterAppServer(), "codex", agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}, claudestream.WithoutPreflight())
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
		_ = svc.Manager.Stop(context.Background(), "s1")
		_ = svc.Manager.Shutdown(context.Background())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.SendTurn(ctx, "s1", "first turn"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	intended, _ := output.CurrentTurn()
	result, err := svc.CancelTurnAndWait(ctx, "s1", "msg://user/local/alice")
	if err != nil {
		t.Fatal(err)
	}
	if result.TurnID == "" || result.TurnID != intended || result.OutputTurnID != intended {
		t.Fatalf("completion = %+v, intended %s", result, intended)
	}
	if err := svc.SendTurn(ctx, "s1", "reply after interruption"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-nextDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	outputs := outputEvents(t, svc)
	if len(outputs) != 2 || outputs[0].TurnID != intended || outputs[1].TurnID == intended {
		t.Fatalf("completed turns = %+v", outputs)
	}
	_ = svc.Manager.Stop(context.Background(), "s1")
	_ = svc.Manager.Shutdown(context.Background())
	if len(fake.Calls()) != 1 {
		t.Fatal("interruption restarted the Codex process")
	}
}

func TestCancelTurnAndWaitCapturedClaudeThenNextTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtimes.Claude, providertest.Replay("claude/stream_interrupt"))
	rt, err := claudestream.NewWithAdapter(&launch.Plan{Command: fake.Path}, gop.NewClaudeAdapterStreamingStdio(), "claude", agentsessions.Capabilities{StreamingStdio: true, BinaryRequired: true}, claudestream.WithoutPreflight())
	if err != nil {
		t.Fatal(err)
	}
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	svc.Manager = agentsessions.NewManager(nil)
	working, finished := make(chan struct{}), make(chan struct{})
	var once, doneOnce sync.Once
	opts := agentsessions.StartOptions{Workdir: t.TempDir(), LogPath: filepath.Join(t.TempDir(), "session.log")}
	output.wire(rt, &opts)
	feed := opts.TypedEventCallback
	opts.TypedEventCallback = func(ev gopevents.Event) {
		feed(ev)
		if _, ok := ev.(gopevents.ToolUse); ok {
			once.Do(func() { close(working) })
		}
		if done, ok := ev.(gopevents.Done); ok && done.StopReason == "end_turn" {
			doneOnce.Do(func() { close(finished) })
		}
	}
	ctx := interruptTestContext(t)
	if err := svc.Manager.Start(ctx, agentsessions.StartRequest{ID: "s1", Runtime: rt, Options: opts}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Manager.Stop(context.Background(), "s1")
		_ = svc.Manager.Shutdown(context.Background())
	})
	if err := svc.SendTurn(ctx, "s1", "Run the shell command `ping -c 30 127.0.0.1` and reply with its last line only."); err != nil {
		t.Fatal(err)
	}
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	result, err := svc.CancelTurnAndWait(ctx, "s1", "actor")
	if err != nil || result.OutputKind == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := svc.SendTurn(ctx, "s1", "say bye"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if len(outputEvents(t, svc)) != 2 || len(fake.Calls()) != 1 {
		t.Fatal("Claude next turn did not reuse process")
	}
}

func TestCancelTurnAndWaitACPThenNextTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtimes.Copilot, providertest.Script(
		providertest.Recv(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`),
		providertest.Send(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}}`),
		providertest.Recv(`{"jsonrpc":"2.0","id":2,"method":"session/new"}`),
		providertest.Send(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"interrupt-session"}}`),
		providertest.Recv(`{"jsonrpc":"2.0","id":3,"method":"session/prompt"}`),
		providertest.Send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"interrupt-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"running"}}}}`),
		providertest.Recv(`{"jsonrpc":"2.0","method":"session/cancel"}`),
		providertest.Send(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`), //nolint:misspell // ACP wire value
		providertest.Recv(`{"jsonrpc":"2.0","id":4,"method":"session/prompt"}`),
		providertest.Send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"interrupt-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"next reply"}}}}`),
		providertest.Send(`{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`),
		providertest.AwaitEOF(), providertest.Exit(0),
	))
	rt, err := acp.New("copilot", "copilot", runtimes.ModeACPStdio, fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	svc.Manager = agentsessions.NewManager(nil)
	working, finished := make(chan struct{}), make(chan struct{})
	var once, doneOnce sync.Once
	terminals := 0
	opts := agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir(), LogPath: filepath.Join(t.TempDir(), "session.log")}
	output.wire(rt, &opts)
	rt.(interface {
		SetEventObserver(func(runtimeevents.Event))
	}).SetEventObserver(func(ev runtimeevents.Event) {
		output.observeRuntime(ev)
		if ev.Kind == runtimeevents.KindAgentDelta {
			once.Do(func() { close(working) })
		}
		if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
			terminals++
			if terminals == 2 {
				doneOnce.Do(func() { close(finished) })
			}
		}
	})
	ctx := interruptTestContext(t)
	if err := svc.Manager.Start(ctx, agentsessions.StartRequest{ID: "s1", Runtime: rt, Options: opts}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Manager.Stop(context.Background(), "s1")
		_ = svc.Manager.Shutdown(context.Background())
	})
	if err := svc.SendTurn(ctx, "s1", "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	result, err := svc.CancelTurnAndWait(ctx, "s1", "actor")
	if err != nil || result.OutputKind == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := svc.SendTurn(ctx, "s1", "next reply"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if len(outputEvents(t, svc)) != 2 || len(fake.Calls()) != 1 {
		t.Fatal("ACP next turn did not reuse process")
	}
}
