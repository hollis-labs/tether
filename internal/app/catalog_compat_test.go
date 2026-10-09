package app

import (
	"context"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// The provider shapes the live ~/.tether/catalog carries (CW-20261001-0065).
// The catalog deploys independently of the binary, so each must keep
// resolving to the runtime it did before the agentkit v0.12.0 registry bump,
// with no catalog edit. codex-cli in particular names no runtime_kind: it must
// stay exec even though the registry's default codex mode is now app-server.
func TestRuntimeFactory_LiveCatalogShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider config.Provider
		wantCaps string
	}{
		{name: "claude-code", provider: config.Provider{ID: "claude-code", Type: "cli", Bootstrap: config.BootstrapSpec{Mode: "streaming-stdio"}}, wantCaps: "streaming-stdio"},
		{name: "claude-stream", provider: config.Provider{ID: "claude-stream", Type: "cli", Bootstrap: config.BootstrapSpec{Mode: "streaming-stdio"}}, wantCaps: "streaming-stdio"},
		{name: "claude-pty", provider: config.Provider{ID: "claude-pty", Type: "cli", Provider: "claude", RuntimeKind: "pty"}, wantCaps: "pty"},
		{name: "codex-app-server", provider: config.Provider{ID: "codex-app-server", Type: "cli", Bootstrap: config.BootstrapSpec{Mode: "jsonrpc-stdio"}}, wantCaps: "jsonrpc-stdio"},
		{name: "codex-cli", provider: config.Provider{ID: "codex-cli", Type: "cli-goprovider", Adapter: "codex", Bootstrap: config.BootstrapSpec{Mode: "agents_md"}}, wantCaps: "subprocess"},
		{name: "opencode", provider: config.Provider{ID: "opencode", Type: "cli", Args: []string{"run"}, Bootstrap: config.BootstrapSpec{Mode: "prepend"}}, wantCaps: "subprocess"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory, err := runtimeFactoryForProvider(tc.provider)
			if err != nil {
				t.Fatalf("runtimeFactoryForProvider: %v", err)
			}
			rt, err := factory(&launch.Plan{Command: "/bin/sh", Args: tc.provider.Args})
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			c := rt.Caps()
			got := "subprocess"
			switch {
			case c.StreamingStdio:
				got = "streaming-stdio"
			case c.JsonRpcStdio:
				got = "jsonrpc-stdio"
			case c.PTY:
				got = "pty"
			}
			if got != tc.wantCaps {
				t.Fatalf("runtime = %s, want %s", got, tc.wantCaps)
			}
		})
	}
}

// opencode.yaml declares `args: [run]`. agentkit v0.12.0's providerplant
// refuses a positional at the head of the flags it appends after the
// projected argv (ErrPositionalAfterProjection); launch.CatalogFlags drops
// the "run" the projection already emits, so the live catalog needs no edit.
func TestPrepareSharedLaunch_OpencodeCatalogRunArg(t *testing.T) {
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
	}
	ws := t.TempDir()
	plan := &launch.Plan{
		LaunchID:       "demo",
		ProjectID:      "project",
		LogicalAgentID: "agent",
		ProviderID:     "opencode",
		ProviderBrand:  "opencode",
		RuntimeKind:    config.RuntimeKindSubprocess,
		RepoRoot:       t.TempDir(),
		WriteHome:      ws,
		WorkspaceMode:  "shared",
		Command:        "opencode",
		Args:           []string{"run"},
		BootPrompt:     "boot",
	}
	prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{ArtifactAdmission: testArtifactAdmission(t, plan), TetherCommand: "tether"})
	if err != nil {
		t.Fatalf("prepareSharedLaunch with args [run]: %v", err)
	}
	if n := countToken(prepared.Argv, "run"); n != 1 {
		t.Fatalf("prepared argv has %d \"run\", want 1: %q", n, prepared.Argv)
	}
	if !slices.Contains(prepared.Argv, "--format") {
		t.Fatalf("prepared argv lost the projected convention: %q", prepared.Argv)
	}
}
