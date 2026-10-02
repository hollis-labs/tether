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
	"github.com/hollis-labs/go-providers/providertest"
	"github.com/hollis-labs/tether/internal/launch"
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
