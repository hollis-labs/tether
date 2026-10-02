package claudestream

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-providers/providertest"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestScopedInterruptInterfaces(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adapter     gop.CLIAdapter
		stream, rpc bool
	}{
		{"claude", gop.NewClaudeAdapterStreamingStdio(), true, false},
		{"codex", gop.NewCodexAdapterAppServer(), false, true},
		{"antigravity", gop.NewAntigravityAdapter(), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := (&PlanScopedAdapter{Inner: tc.adapter}).withInterrupts()
			_, stream := a.(gop.TurnInterrupter)
			_, rpc := a.(gop.RPCTurnInterrupter)
			if stream != tc.stream || rpc != tc.rpc {
				t.Fatalf("interrupt interfaces stream=%v rpc=%v", stream, rpc)
			}
		})
	}
}

func TestScopedCodexInterruptCapturedTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtimes.Codex, providertest.Replay("codex/app_server_interrupt"))
	rt, err := NewWithAdapter(&launch.Plan{Command: fake.Path}, gop.NewCodexAdapterAppServer(), "codex", agentsessions.Capabilities{JsonRpcStdio: true, BinaryRequired: true}, WithoutPreflight())
	if err != nil {
		t.Fatal(err)
	}
	notes := make(chan string, 256)
	statuses := make(chan string, 8)
	dir := t.TempDir()
	sess, err := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: dir, LogPath: filepath.Join(dir, "session.log"), JsonRpcNotificationHook: func(method string, params json.RawMessage) {
		notes <- method
		if method == "turn/completed" {
			var p struct {
				Turn struct {
					Status string `json:"status"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(params, &p)
			statuses <- p.Turn.Status
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Stop(context.Background()); _, _ = sess.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	caller := sess.(agentsessions.JsonRpcCaller)
	for _, method := range []string{"initialize", "thread/start", "turn/start"} {
		if _, err := caller.Call(ctx, method, map[string]any{}); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	for {
		select {
		case method := <-notes:
			if method == "item/started" {
				goto running
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
running:
	if err := sess.(agentsessions.TurnInterrupter).InterruptTurn(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-statuses:
		if status != "interrupted" {
			t.Fatalf("status %q", status)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := caller.Call(ctx, "turn/start", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-statuses:
		if status != "completed" {
			t.Fatalf("next status %q", status)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_ = sess.Stop(context.Background())
	_, _ = sess.Wait()
	if len(fake.Calls()) != 1 {
		t.Fatal("interrupt restarted the provider process")
	}
	var frame string
	for _, line := range fake.Call(0).Stdin {
		if strings.Contains(line, `"turn/interrupt"`) {
			frame = line
		}
	}
	if !strings.Contains(frame, `"turnId":"00000000-0000-4000-8000-000000000003"`) {
		t.Fatalf("interrupt did not target the captured open turn: %s", frame)
	}
}
