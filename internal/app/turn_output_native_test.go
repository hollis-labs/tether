package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/adapters/providertest"
	"github.com/hollis-labs/substrate/harness/adapters/turnoutput"
	"github.com/hollis-labs/substrate/llm-core/contracts/runtimes"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

// Real agentkit transports, fake captured CLIs: output must reach the reducer
// through the launch-installed callback, then persist once before emission.
func TestNativeRuntimeTurnOutputThroughLaunch(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		runtime                 runtimes.ID
		mode, fixture, wantText string
	}{
		{"claude streaming", runtimes.Claude, "streaming-stdio", "claude/stream_resume", "Bye! 👋"},
		{"codex exec", runtimes.Codex, "subprocess", "codex/exec_turn1", "Hi!"},
		{"codex app-server", runtimes.Codex, "jsonrpc-stdio", "codex/app_server_turn", "Hi!"},
		{"opencode run", runtimes.OpenCode, "subprocess", "opencode/run_turn1", "OK."},
		{"antigravity", runtimes.Antigravity, "subprocess", "antigravity/print_turn1", "OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, id := nativeOutputLaunch(t, tc.runtime, tc.mode, providertest.Replay(tc.fixture))
			db := svc.Store
			turns := 1
			if tc.mode == "jsonrpc-stdio" {
				turns = 2
			}
			var outputs []events.TurnOutputEvent
			for turn := 1; turn <= turns; turn++ {
				prompt := "say hi"
				if turn == 2 {
					prompt = "say bye"
				}
				if err := svc.SendTurn(context.Background(), id, prompt); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(30 * time.Second)
				for time.Now().Before(deadline) {
					outputs = outputEvents(t, svc)
					if len(outputs) >= turn {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if len(outputs) != turn || outputs[turn-1].MessageID == "" || outputs[turn-1].Kind != turnoutput.KindFinal || outputs[turn-1].Confidence != turnoutput.ConfidenceExact {
					t.Fatalf("transport output after turn %d: %+v", turn, outputs)
				}
				if turn == 2 && outputs[0].TurnID == outputs[1].TurnID {
					t.Fatal("turn identity reused")
				}
			}
			saved, err := db.StagedTurnOutput(context.Background(), outputs[0].MessageID)
			var body struct {
				Text string `json:"text"`
			}
			if err == nil {
				err = json.Unmarshal(saved.Payload, &body)
			}
			if err != nil || body.Text != tc.wantText {
				t.Fatalf("staged answer: %+v %v", saved, err)
			}
		})
	}
}

func nativeOutputLaunch(t *testing.T, runtime runtimes.ID, mode string, run providertest.Run) (*Service, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtime, run)
	prov := config.Provider{ID: string(runtime), Type: "cli", Command: fake.Path, RuntimeKind: mode}
	factory, err := runtimeFactoryForProvider(prov)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "output.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id := "native-output"
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
	t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), id) })

	return svc, id
}

func TestNativeSubprocessSyntheticFailureTurnOutput(t *testing.T) {
	svc, id := nativeOutputLaunch(t, runtimes.Codex, "subprocess", providertest.Script(providertest.Stderr("fixture process failure"), providertest.Exit(1)))
	_ = svc.SendTurn(context.Background(), id, "fail this turn")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		outputs := outputEvents(t, svc)
		if len(outputs) > 0 {
			if len(outputs) != 1 || outputs[0].Kind != turnoutput.KindFailure {
				t.Fatalf("failure outputs: %+v", outputs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("subprocess failure produced no turn output while session stayed reusable")
}
