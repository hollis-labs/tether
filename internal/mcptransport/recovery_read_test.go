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
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRecoveryReadExistingPoolAndAuthority(t *testing.T) {
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "recovery-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "tesseract_recall", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"unexpected": true}}, nil
	})
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
	cat := &config.Catalog{MCPServerEnabled: map[string]bool{"engine": true, "duplicate": true}}
	pool, err := mcpadapter.NewSharedUpstreams([]config.MCPServerEntry{{ID: "engine", Transport: "http", URL: remote.URL, AllowUnconfinedRemote: true}, {ID: "duplicate", Transport: "http", URL: remote.URL, AllowUnconfinedRemote: true}}, mcpadapter.DaemonProtectedRoots{Catalog: filepath.Join(root, "catalog"), Run: filepath.Join(root, "run"), State: filepath.Join(root, "state"), CatalogConfig: cat}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := mcpgateway.SessionPolicy{SessionID: "session", Servers: []string{"engine"}}.Seal()
	h := &Handler{ctx: ctx, cancel: cancel, cfg: HandlerConfig{Resolver: CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { return cat, nil }, RecoverySession: func(_ context.Context, id string) (mcpgateway.SessionPolicy, error) {
		if id != "session" {
			return mcpgateway.SessionPolicy{}, ErrRecoveryReadForbidden
		}
		return policy, nil
	}}}}
	if _, err = h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadUnavailable) {
		t.Fatal("absent runtime", err)
	}
	// Only host setup activates the pool. The helper below reuses this connection.
	if err = pool.StartOrigins(ctx, []string{"engine"}); err != nil {
		t.Fatal(err)
	}
	h.runtime = pool
	if _, err = h.ReadRecoveryTool(ctx, "session", "tesseract", "tesseract_recall", nil); !errors.Is(err, ErrRecoveryReadForbidden) {
		t.Fatal("missing read-only annotation accepted", err)
	}
	raw, err := h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", map[string]any{"id": "task"})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || string(result["source"]) != `"existing-runtime"` {
		t.Fatal("lost result envelope")
	}
	for _, test := range []struct {
		ctx                  context.Context
		source, origin, tool string
	}{
		{ctx, "", "torque", "torque_task_get"},
		{ctx, "session", "torque", "torque_task_update"},
		{ctx, "session", "tesseract", "torque_task_get"},
		{ctx, "ungranted", "torque", "torque_task_get"},
	} {
		if _, err = h.ReadRecoveryTool(test.ctx, test.source, test.origin, test.tool, nil); !errors.Is(err, ErrRecoveryReadForbidden) {
			t.Fatal("unauthorized read", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unauthorized call reached upstream")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err = h.ReadRecoveryTool(canceled, "session", "torque", "torque_task_get", nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read dispatched", err)
	}
	// A launch floor still restricts a currently permissive catalog profile.
	profile := "restricted"
	cat.Global.MCP.Profiles = map[string]mcpgateway.Profile{profile: {}}
	policy = mcpgateway.SessionPolicy{SessionID: "session", Servers: []string{"engine"}, Profile: &profile, LaunchProfile: &mcpgateway.Profile{Tools: mcpgateway.ToolRules{Deny: []string{"torque_task_get"}}}}.Seal()
	if _, err = h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadForbidden) {
		t.Fatal("launch floor widened", err)
	}
	if calls.Load() != 1 {
		t.Fatal("excluded call reached upstream")
	}
	policy = mcpgateway.SessionPolicy{SessionID: "session", Servers: []string{"engine"}}.Seal()
	cat.MCPServerEnabled["engine"] = false
	if _, err = h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadForbidden) {
		t.Fatal("disabled current origin accepted", err)
	}
	cat.MCPServerEnabled["engine"] = true
	policy = mcpgateway.SessionPolicy{SessionID: "other", Servers: []string{"engine"}}.Seal()
	if _, err = h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadForbidden) {
		t.Fatal("different source policy accepted", err)
	}
	policy = mcpgateway.SessionPolicy{SessionID: "session", Servers: []string{}}.Seal()
	if _, err = h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadForbidden) {
		t.Fatal("zero grant widened", err)
	}
	// A second initialized origin advertising the same final name cannot
	// make an ambiguous tool eligible for a trusted boot read.
	_ = pool.StartOrigins(ctx, []string{"duplicate"})
	policy = mcpgateway.SessionPolicy{SessionID: "session", Servers: []string{"engine", "duplicate"}}.Seal()
	if _, err = h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", nil); err == nil {
		t.Fatal("ambiguous read dispatched")
	}
	if calls.Load() != 1 {
		t.Fatal("refused call reached upstream")
	}
	h.closed = true
	if _, err = h.ReadRecoveryTool(ctx, "session", "torque", "torque_task_get", nil); !errors.Is(err, ErrRecoveryReadUnavailable) {
		t.Fatal("closed handler", err)
	}
}
