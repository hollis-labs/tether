//go:build linux

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/llm-core/contracts/runtimes"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/provider/cli/antigravity"
	"github.com/hollis-labs/tether/internal/shimagy"
	"github.com/hollis-labs/tether/internal/shimhost"
	"golang.org/x/sys/unix"
)

func TestShimAGYWorkerProcess(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--config" && i+1 < len(os.Args) {
			parent := os.Getppid()
			if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0) != nil || os.Getppid() != parent {
				os.Exit(96)
			}
			var cfg shimagy.Config
			if json.Unmarshal([]byte(os.Args[i+1]), &cfg) != nil {
				os.Exit(2)
			}
			if shimagy.Run(context.Background(), cfg, os.Stdin, os.Stdout, os.Stderr) != nil {
				os.Exit(3)
			}
			os.Exit(0)
		}
	}
}

func TestShimAGYMidTurnDetachReattachAndNextNativeTurn(t *testing.T) {
	f := shimFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.svc.shimHosting.agyWorker = []string{exe, "-test.run=^TestShimAGYWorkerProcess$", "--"}
	agy := filepath.Join(f.root, "agy")
	gate := filepath.Join(f.root, "finish-turn")
	const script = `#!/bin/sh
if [ "$1" = "--conversation" ]; then
  [ "$2" = "retained-native" ] || exit 4
else
  printf '%s\n' started > "$AGY_STARTED"
fi
printf '%s\n' '{"event":"init","conversation_id":"retained-native","init":{}}'
printf '%s\n' '{"event":"step_update","step_update":{"step_index":1,"step_type":"agent_response","state":"ACTIVE","text_delta":"partial answer"}}'
while [ ! -f "$AGY_GATE" ]; do sleep 0.02; done
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"same answer"}}'
`
	if err = os.WriteFile(agy, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.plan.Command, f.plan.ProviderBrand, f.plan.ProviderID, f.plan.RuntimeKind = agy, "antigravity", "antigravity", "subprocess"
	f.req.Runtime, err = antigravity.New(f.plan)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.turnFeeds = map[string]turnFeedRegistration{"antigravity": {}}
	encoded, _ := json.Marshal(f.plan)
	if _, err = f.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(encoded), f.req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Store.DB().Exec(`UPDATE sessions SET provider_id=? WHERE id=?`, "antigravity", f.req.ID); err != nil {
		t.Fatal(err)
	}
	f.req.Options.Env = append(f.req.Options.Env, "AGY_GATE="+gate, "AGY_STARTED="+filepath.Join(f.root, "started"))
	f.req.Options.Launch = &agentlaunch.TurnTemplate{Convention: gop.LaunchConvention{Executable: agy, Mode: runtimes.ModeSubprocessPerTurn, Argv: []gop.ArgTemplate{{Kind: gop.ArgResume, Value: "--conversation"}, {Kind: gop.ArgPromptInline, Value: "-p="}}}}
	f.req.Options.OnSessionID = func(id string) { _ = f.svc.Store.UpsertSessionProviderMapping(f.req.ID, "tether", "antigravity", id) }
	req, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil || req.Runtime == f.req.Runtime || !req.Runtime.Caps().StreamingStdio {
		t.Fatalf("AGY shim preparation: %v", err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	output := f.svc.newSessionTurnOutput(*row, f.plan)
	output.wire(req.Runtime, &req.Options)
	f.svc.turnOutputs.Store(f.req.ID, output)
	if err = f.svc.Manager.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	f.svc.leaseActorBinding(f.req.ID, "worker")
	f.svc.watchSessionBindings(f.req.ID)
	if err = f.svc.SendTurn(context.Background(), f.req.ID, "before restart"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "genuine AGY turn start", func() bool {
		mapping, e := f.svc.Store.GetSessionProviderMapping(f.req.ID, "tether", "antigravity")
		return e == nil && mapping.NativeSessionID.String == "retained-native"
	})
	shimRow, _ := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	receipt, err := loadShimReceipt(shimRow)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputEvents(t, f.svc)) != 0 {
		t.Fatal("pending native turn received terminal delivery credit")
	}
	if err = f.svc.SendTurn(context.Background(), f.req.ID, "queued while active"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = f.svc.DrainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(gate, []byte("finish"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, stops := newSessionManager(f.svc.Store, f.svc.Bus)
	restarted := &Service{Catalog: f.svc.Catalog, Store: f.svc.Store, Bus: f.svc.Bus, Registry: f.svc.Registry, Manager: manager, stops: stops, shimHosting: f.svc.shimHosting, turnFeeds: f.svc.turnFeeds}
	t.Cleanup(func() { _ = restarted.StopSession(f.req.ID); _ = manager.Shutdown(context.Background()) })
	restarted.ReconcileStaleState()
	shimAwait(t, "detached and queued turn completion from canonical journal", func() bool { return len(outputEvents(t, restarted)) >= 2 })
	if err = restarted.SendTurn(context.Background(), f.req.ID, "after restart"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "next turn under retained conversation", func() bool { return len(outputEvents(t, restarted)) >= 3 })
	if outputs := outputEvents(t, restarted); len(outputs) != 3 {
		t.Fatalf("unexpected replayed output: %+v", outputs)
	}
	shimRow, _ = restarted.Store.SessionShim(context.Background(), f.req.ID)
	after, err := loadShimReceipt(shimRow)
	if err != nil || after.HostPID != receipt.HostPID || after.ProviderPID != receipt.ProviderPID || after.Journal != receipt.Journal {
		t.Fatalf("reattach replaced hosted custody: %v", err)
	}
	mapping, err := restarted.Store.GetSessionProviderMapping(f.req.ID, "tether", "antigravity")
	if err != nil || mapping.NativeSessionID.String != "retained-native" {
		t.Fatal("native conversation was replaced")
	}
}

func TestShimAGYResultReplayRequiresExactPublishedIdentity(t *testing.T) {
	f := shimFixture(t)
	payload, _ := json.Marshal(events.TurnOutputEvent{SessionID: f.req.ID, TurnID: "first", ProviderResultID: "published"})
	if err := f.svc.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: f.req.ID, Kind: events.KindSessionTurnOutput, PayloadJSON: string(payload)}); err != nil {
		t.Fatal(err)
	}
	adapter := &shimAGYAdapter{AntigravityAdapter: gop.NewAntigravityAdapter(), service: f.svc, sessionID: f.req.ID}
	for _, id := range []string{"published", "different", ""} {
		line, _ := json.Marshal(map[string]any{"event": "result", "uuid": id, "result": map[string]string{"status": "SUCCESS", "response": "same text"}})
		legacy, err := adapter.ParseLine(line)
		typed, typedErr := adapter.ParseLineEvents(line)
		if typedErr != nil {
			t.Fatal(typedErr)
		}
		if id == "different" {
			if err != nil || len(legacy) == 0 || len(typed) == 0 {
				t.Fatal("new turn suppressed by matching text")
			}
		} else if len(legacy) != 0 || len(typed) != 0 || (id == "" && err == nil) {
			t.Fatal("published or unidentified terminal result admitted")
		}
	}
}

func TestShimAGYBootInputFramingAndBoundedLaunch(t *testing.T) {
	opts := shimFixture(t).req.Options
	opts.BootMode, opts.BootPrompt = "stdin", "line one\nline two"
	opts.FirstTurnPayload = []byte("kickoff\nnext line")
	opts, err := agyBridgeOptions(opts, []string{"/bin/bridge"}, shimhost.Receipt{}, false)
	if err != nil || strings.Count(opts.BootPrompt, "\n") != 1 || !json.Valid([]byte(strings.TrimSpace(opts.BootPrompt))) || !json.Valid(opts.FirstTurnPayload) {
		t.Fatalf("unframed AGY boot input: %v", err)
	}
	opts.Launch.Convention.Mode = runtimes.ModeSubprocessPerTurn
	if _, err = agyWorkerArgv("/bin/agy", opts, strings.Repeat("x", shimagy.MaxConfig), nil); err == nil {
		t.Fatal("oversized worker argument admitted")
	}
}
