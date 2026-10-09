package mcptransport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRecoveryReadExistingPoolAndAuthority(t *testing.T) {
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "recovery-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "torque_task_get", InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"source": "existing-runtime"}, Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
	})
	remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer remote.Close()
	root := t.TempDir()
	for _, name := range []string{"catalog", "run", "state"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cat := &config.Catalog{MCPServerEnabled: map[string]bool{"torque": true}}
	cat.Global.Identity.MCPGrants = map[string]config.PrincipalMCPGrant{"reader": {Servers: []string{"torque"}}}
	pool, err := mcpadapter.NewSharedUpstreams([]config.MCPServerEntry{{ID: "torque", Transport: "http", URL: remote.URL, AllowUnconfinedRemote: true}}, mcpadapter.DaemonProtectedRoots{Catalog: filepath.Join(root, "catalog"), Run: filepath.Join(root, "run"), State: filepath.Join(root, "state"), CatalogConfig: cat}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Handler{ctx: ctx, cancel: cancel, cfg: HandlerConfig{Resolver: CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { return cat, nil }}}}
	verified := identity.WithPrincipal(ctx, identity.Principal{ID: "reader", Kind: "service"})
	if _, err = h.ReadRecoveryTool(verified, "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadUnavailable) {
		t.Fatal("absent runtime", err)
	}
	// Only host setup activates the pool. The helper below reuses this connection.
	if err = pool.StartOrigins(ctx, []string{"torque"}); err != nil {
		t.Fatal(err)
	}
	h.runtime = pool
	raw, err := h.ReadRecoveryTool(verified, "torque", "torque_task_get", map[string]any{"id": "task"})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || len(result["structuredContent"]) == 0 {
		t.Fatal("lost result envelope")
	}
	for _, test := range []struct {
		ctx          context.Context
		origin, tool string
	}{
		{ctx, "torque", "torque_task_get"},
		{verified, "torque", "torque_task_update"},
		{verified, "tesseract", "torque_task_get"},
		{identity.WithPrincipal(ctx, identity.Principal{ID: "ungranted", Kind: "service"}), "torque", "torque_task_get"},
	} {
		if _, err = h.ReadRecoveryTool(test.ctx, test.origin, test.tool, nil); !errors.Is(err, ErrRecoveryReadForbidden) {
			t.Fatal("unauthorized read", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unauthorized call reached upstream")
	}
	// A launch floor still restricts a currently permissive catalog profile.
	profile := "restricted"
	cat.Global.MCP.Profiles = map[string]mcpgateway.Profile{profile: {}}
	policy := mcpgateway.SessionPolicy{SessionID: "session", Servers: []string{"torque"}, Profile: &profile, LaunchProfile: &mcpgateway.Profile{Tools: mcpgateway.ToolRules{Deny: []string{"torque_task_get"}}}}.Seal()
	h.cfg.Resolver.Session = func(context.Context, string) (mcpgateway.SessionPolicy, error) { return policy, nil }
	if _, err = h.ReadRecoveryTool(identity.WithPrincipal(ctx, identity.Principal{ID: "session-principal", Kind: "session", SessionID: "session"}), "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadForbidden) {
		t.Fatal("launch floor widened", err)
	}
	if calls.Load() != 1 {
		t.Fatal("excluded call reached upstream")
	}
	h.closed = true
	if _, err = h.ReadRecoveryTool(verified, "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadUnavailable) {
		t.Fatal("closed handler", err)
	}
}
