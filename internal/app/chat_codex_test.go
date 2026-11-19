package app

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	"github.com/hollis-labs/libs/harness/core/adapters/providertest"
	"github.com/hollis-labs/libs/llm/core/contracts/runtimes"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatwire"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
)

// The real Manager and JSON-RPC reader, one fake provider process, and the
// Service's normal cached thread/send/approval paths. Notifications deliberately
// precede the turn/start response; the request reader must remain unblocked.
func TestCodexChatExistingSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	begin := `{"method":"turn/started","params":{"threadId":"chat-thread","turn":{"id":"chat-turn","status":"inProgress"}}}`
	end := `{"method":"turn/completed","params":{"threadId":"chat-thread","turn":{"id":"chat-turn","status":"completed"}}}`
	steps := []providertest.Step{
		providertest.Recv(`{"id":1,"method":"initialize"}`), providertest.Send(`{"id":1,"result":{"userAgent":"fake"}}`),
		providertest.Recv(`{"id":2,"method":"thread/start"}`), providertest.Send(`{"id":2,"result":{"thread":{"id":"chat-thread"}}}`),
	}
	for i := 0; i < 2; i++ {
		steps = append(steps,
			providertest.Recv(`{"id":3,"method":"turn/start"}`), providertest.Stdout(begin),
			providertest.Stdout(`{"method":"item/started","params":{"threadId":"chat-thread","turnId":"chat-turn","item":{"type":"userMessage","content":[{"type":"text","text":"private-integration-context"}]}}}`),
			providertest.Stdout(`{"method":"item/agentMessage/delta","params":{"threadId":"chat-thread","turnId":"chat-turn","itemId":"answer","delta":"done"}}`),
			providertest.Stdout(`{"id":97,"method":"mcpServer/elicitation/request","params":{"threadId":"chat-thread","turnId":"chat-turn","serverName":"app","_meta":{"codex_approval_kind":"mcp_tool_call"}}}`),
			providertest.Recv(`{"id":97,"result":{"action":"accept","content":{}}}`),
			providertest.Stdout(end), providertest.Send(`{"id":3,"result":{"turn":{"id":"chat-turn"}}}`),
		)
	}
	steps = append(steps, providertest.AwaitEOF())
	fake := providertest.New(t, runtimes.Codex, providertest.Script(steps...))
	svc, backend, ctx := codexChatHarness(t, fake, nil)
	for i := 0; i < 2; i++ {
		run, err := backend.Submit(ctx, chatbackend.Turn{Text: "hello", Context: json.RawMessage(`{"view":"private-integration-context"}`)})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		delta, activity := false, false
		for e, err := range run.Events() {
			if err != nil {
				t.Fatal(err)
			}
			if e.IsTerminal() {
				n++
				if e.Verb != chatstream.VerbRunFinish {
					t.Fatalf("terminal %+v", e)
				}
			}
			if e.Verb == chatstream.VerbPartDelta && e.Text == "done" {
				delta = true
			}
			if e.Verb == chatstream.VerbActivity {
				activity = true
			}
			raw, _ := json.Marshal(e)
			if bytes.Contains(raw, []byte("private-integration-context")) {
				t.Fatalf("context in events: %s", raw)
			}
		}
		if n != 1 || !delta || !activity {
			t.Fatalf("terminal %d, delta %v, approval activity %v", n, delta, activity)
		}
	}
	_ = svc.Manager.Stop(ctx, "s1")
	_ = svc.Manager.Shutdown(ctx)
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatal("chat backend launched another Codex process")
	}
	counts := map[string]int{}
	for _, line := range calls[0].Stdin {
		var frame struct {
			Method string `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &frame) != nil {
			t.Fatal("invalid client frame")
		}
		counts[frame.Method]++
		if frame.Method == "turn/start" && (frame.Params.ThreadID != "chat-thread" || len(frame.Params.Input) != 1 || frame.Params.Input[0].Text != "<app-context>{\"view\":\"private-integration-context\"}</app-context>\nhello") {
			t.Fatal("wrong native thread or private input framing")
		}
	}
	if counts["initialize"] != 1 || counts["thread/start"] != 1 || counts["turn/start"] != 2 {
		t.Fatalf("protocol call counts: %v", counts)
	}
}

func codexChatHarness(t *testing.T, fake *providertest.Fake, hook func(string, json.RawMessage)) (*Service, chatwire.Backend, context.Context) {
	t.Helper()
	rt, err := claudestream.NewWithAdapter(&launch.Plan{Command: fake.Path}, gop.NewCodexAdapterAppServer(), "codex", agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}, claudestream.WithoutPreflight())
	if err != nil {
		t.Fatal(err)
	}
	svc, output := outputHarness(t, nil)
	svc.turnOutputs.Store("s1", output)
	svc.Manager = agentsessions.NewManager(nil)
	dir := t.TempDir()
	opts := agentsessions.StartOptions{Workdir: dir, LogPath: filepath.Join(dir, "session.log"), JsonRpcRequestHook: jsonRPCRequestHook("s1"), JsonRpcNotificationHook: hook}
	output.wire(rt, &opts)
	tap := svc.installCodexTap("s1", "codex", rt, &opts)
	if tap == nil {
		t.Fatal("missing local tap")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := svc.Manager.Start(ctx, agentsessions.StartRequest{ID: "s1", Runtime: rt, Options: opts}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.Manager.Stop(context.Background(), "s1")
		_ = svc.Manager.Shutdown(context.Background())
		tap.Close()
	})
	// It attaches to the already running session. Creating the backend cannot
	// launch a provider and the second turn must reuse the first thread.
	backend, err := svc.ChatBackend("s1", chatwire.RuntimeCodexAppServer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- backend.Run(ctx) }()
	<-backend.(interface{ Ready() <-chan struct{} }).Ready()
	t.Cleanup(func() { cancel(); <-done })
	return svc, backend, ctx
}

// Captured app-server RPCs prove Cancel targets the vendor's exact thread/turn
// and the next turn keeps both the process and the native thread.
func TestCodexChatCancelExistingSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtimes.Codex, providertest.Replay("codex/app_server_interrupt"))
	working := make(chan struct{}, 1)
	svc, be, ctx := codexChatHarness(t, fake, func(method string, _ json.RawMessage) {
		if method == "item/started" {
			select {
			case working <- struct{}{}:
			default:
			}
		}
	})
	first, err := be.Submit(ctx, chatbackend.Turn{Text: "first turn"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	first.Cancel()
	check := func(run chatbackend.Run, want chatstream.Verb) {
		t.Helper()
		n := 0
		for ev, err := range run.Events() {
			if err != nil {
				t.Fatal(err)
			}
			if ev.IsTerminal() {
				n++
				if ev.Verb != want {
					t.Fatalf("terminal %s, want %s", ev.Verb, want)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d terminal events", n)
		}
	}
	check(first, chatstream.VerbRunAbort)
	second, err := be.Submit(ctx, chatbackend.Turn{Text: "reply after interruption"})
	if err != nil {
		t.Fatal(err)
	}
	check(second, chatstream.VerbRunFinish)
	_ = svc.Manager.Stop(ctx, "s1")
	_ = svc.Manager.Shutdown(ctx)
	if len(fake.Calls()) != 1 {
		t.Fatal("Cancel restarted Codex")
	}
}
