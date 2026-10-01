package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"path/filepath"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/providertest"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

// CW-20260930-0106 stage 1: a catalog provider with runtime_kind acp-stdio
// launches through LaunchSession and go-agent-wrapper's launch.Select, with
// no shared-launch planting. The boot prompt is the agent's first prompt; a
// later prompt goes through SendTurn as `mux sessions turn` sends it. Both
// replies land in the session's logs/session.log. The fakes replay captured
// ACP turns, so Copilot and Pi need not be installed.
func TestLaunchSession_ACPProviders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runtime  runtimes.ID
		run      providertest.Run
		bootTurn bool
	}{
		{"copilot boot prompt", runtimes.Copilot, providertest.Replay("copilot/acp_turn").When("--acp"), true},
		{"pi turn", runtimes.Pi, providertest.Replay("pi/acp_turn"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TETHER_ENABLE_ACP", "1")
			fake := providertest.New(t, tc.runtime, tc.run)
			prov := config.Provider{ID: string(tc.runtime), Type: "cli", Command: fake.Path, RuntimeKind: "acp-stdio"}
			factory, err := runtimeFactoryForProvider(prov)
			if err != nil {
				t.Fatalf("runtimeFactoryForProvider: %v", err)
			}
			db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			sessID := "sess-acp-" + string(tc.runtime)
			plan := &launch.Plan{
				LaunchID:       "acp-launch",
				ProjectID:      "proj",
				LogicalAgentID: "agent",
				ProviderID:     prov.ID,
				ProviderBrand:  prov.ProviderBrand(),
				RuntimeKind:    prov.EffectiveRuntimeKind(),
				RepoRoot:       t.TempDir(),
				WriteHome:      t.TempDir(),
				WorkspaceMode:  "shared",
				Command:        fake.Path,
			}
			if tc.bootTurn {
				plan.BootPrompt = "say hi"
			}
			ws, err := workspace.Create(plan.WriteHome, sessID, plan)
			if err != nil {
				t.Fatalf("create workspace: %v", err)
			}
			row := store.SessionRow{
				ID: sessID, LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID,
				ProviderID: plan.ProviderID, ProviderKind: "cli", Workspace: ws.Root, State: "created",
			}
			if err := db.CreateSession(row, plan); err != nil {
				t.Fatalf("create session: %v", err)
			}
			svc := &Service{
				CatalogRoot: t.TempDir(),
				Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
				Store:       db,
				Manager:     agentsessions.NewManager(stateSinkAdapter{db: db}),
				factories:   map[string]RuntimeFactory{prov.ID: factory},
			}
			launched, err := svc.LaunchSession(sessID)
			if err != nil {
				t.Fatalf("LaunchSession: %v", err)
			}
			t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), sessID) })
			if launched.ProviderKind != "acp" {
				t.Fatalf("ProviderKind = %q, want acp", launched.ProviderKind)
			}
			if !tc.bootTurn {
				if err := svc.SendTurn(context.Background(), sessID, "say hi"); err != nil {
					t.Fatalf("SendTurn: %v", err)
				}
			}
			log := waitForLogText(t, ws.LogPath, "[turn_done]")
			if !strings.Contains(log, "Hi!") {
				t.Fatalf("session.log has no reply:\n%s", log)
			}
			if entries, _ := os.ReadDir(filepath.Join(ws.Root, "boot")); len(entries) != 0 {
				t.Errorf("ACP launch planted a shared-launch boot dir: %v", entries)
			}
		})
	}
}

// The ACP gate (launch.ErrACPLaunchDisabled) is closed by default: the
// factory refuses, so neither session create (which probes it) nor
// LaunchSession starts an ACP agent. TETHER_ENABLE_ACP=1 opens it.
func TestACPLaunchGate(t *testing.T) {
	prov := config.Provider{ID: "copilot", Type: "cli", RuntimeKind: "acp-stdio"}
	factory, err := runtimeFactoryForProvider(prov)
	if err != nil {
		t.Fatalf("runtimeFactoryForProvider: %v", err)
	}
	plan := &launch.Plan{ProviderID: "copilot", ProviderBrand: "copilot", RuntimeKind: "acp-stdio"}

	t.Setenv("TETHER_ENABLE_ACP", "")
	if _, err := factory(plan); !errors.Is(err, launch.ErrACPLaunchDisabled) {
		t.Fatalf("factory with the gate closed = %v, want ErrACPLaunchDisabled", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan.LaunchID, plan.ProjectID, plan.WriteHome, plan.RepoRoot = "acp", "proj", t.TempDir(), t.TempDir()
	ws, err := workspace.Create(plan.WriteHome, "sess-gated", plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: "sess-gated", LaunchID: "acp", ProjectID: "proj", ProviderID: "copilot", ProviderKind: "acp", Workspace: ws.Root, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Manager: agentsessions.NewManager(stateSinkAdapter{db: db}), factories: map[string]RuntimeFactory{"copilot": factory}}
	if _, err := svc.LaunchSession("sess-gated"); !errors.Is(err, launch.ErrACPLaunchDisabled) {
		t.Fatalf("LaunchSession with the gate closed = %v, want ErrACPLaunchDisabled", err)
	}

	for _, v := range []string{"1", "true"} {
		t.Setenv("TETHER_ENABLE_ACP", v)
		if _, err := factory(plan); err != nil {
			t.Fatalf("factory with TETHER_ENABLE_ACP=%s = %v, want a runtime", v, err)
		}
	}
}

func waitForLogText(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, _ := os.ReadFile(path) //nolint:gosec // test-owned workspace path
		if strings.Contains(string(data), want) {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %s:\n%s", want, path, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
