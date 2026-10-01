package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCodexConfinedProxy_RealDaemonGateway(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("control-plane protection ships on Linux only")
	}
	// Build before isolating HOME so the build uses the existing module cache.
	binary := muxBinary(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	d := StartFixtureDaemon(t)
	protected := []string{d.CatalogDir, filepath.Join(d.StateRoot, "run"), filepath.Dir(d.StateDBPath())}
	plan := &launch.Plan{ProviderBrand: "codex", RepoRoot: t.TempDir()}
	// Exact constructor used by LaunchSession; only the test executable/paths
	// and granted upstream list differ. No model process is ever started.
	planting := app.MuxMCPPlant(d.CatalogDir, "smoke-codex", false, protected...)
	command, args, err := app.ConfineMCPPlant(plan, binary, planting.Args, protected)
	if err != nil {
		t.Fatal(err)
	}
	probeCommand, probeArgs, err := app.ConfineMCPPlant(plan, "/bin/true", nil, protected)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(probeCommand, probeArgs...).CombinedOutput(); err != nil {
		t.Skipf("namespace unavailable: %v: %s", err, output)
	}
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(), "MUX_MCP_SERVERS=", config.MCPConfineRemoteEnv+"=1")
	cmd.Stderr = os.Stderr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "confined-smoke", Version: "test"}, nil).Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "mux_session_list" {
			found = true
		}
	}
	if !found {
		t.Fatal("real confined proxy lost daemon gateway tools")
	}
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "mux_session_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("daemon-backed call: result=%+v err=%v", result, err)
	}
	t.Logf("real protected proxy: tools/list returned %d tools; mux_session_list succeeded over %s with read-only catalog/run/state", len(tools.Tools), d.SocketAddr)
}
