package mcpadapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

func TestProxyCallErrorRedactsHTTPURLAtAgentBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	upstream := gomcp.NewServer("upstream", "test")
	registerTestTool(upstream, "probe")
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream.SDKServer() }, nil))
	defer srv.Close()
	const secret = "upstream-url-token-987654321"
	endpoint := srv.URL + "/" + secret
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(dir, "url-token")
	if err := os.WriteFile(credential, []byte(endpoint), 0600); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("id: remote\ntransport: http\nurl: file://" + credential + "\n")
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers", "remote.yaml"), yaml, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := config.LoadMCPServers(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewToolRegistry()
	pool := NewClientPool(entries, registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := pool.connect(ctx, entries[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	registry.Register("remote", client, []*mcpsdk.Tool{makeTool("probe")})
	pool.statuses["remote"] = &clientStatus{entry: entries[0], client: client, state: "connected"}
	router := NewProxyRouter(registry)
	router.pool = pool
	local := gomcp.NewServer("proxy", "test")
	live := &liveProxyCatalog{adapter: &Adapter{}, server: local, router: router, firehose: true}
	live.addProxyTools(makeTool("probe"))
	downstream := connectInMemory(t, local)
	srv.CloseClientConnections()
	srv.Close()
	result, callErr := downstream.CallTool(ctx, &mcpsdk.CallToolParams{Name: "probe"})
	if callErr == nil && (result == nil || !result.IsError) {
		t.Fatal("dead HTTP upstream unexpectedly succeeded")
	}
	text := fmt.Sprint(callErr)
	if result != nil {
		for _, content := range result.Content {
			if block, ok := content.(*mcpsdk.TextContent); ok {
				text += " " + block.Text
			}
		}
	}
	if strings.Contains(text, secret) || strings.Contains(text, endpoint) {
		t.Fatal("returned call error exposes upstream URL credential")
	}
	if !strings.Contains(text, "[redacted]") {
		t.Fatalf("failure did not exercise URL redaction: %s", text)
	}
	if !strings.Contains(text, "request was not replayed") {
		t.Fatalf("error lost its execution-outcome explanation: %s", text)
	}
}

func TestProxyCallErrorKeepsCauseAndShortValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const secret = "upstream-bearer-secret-0123456789"
	cause := errors.New("connection failed on port 1 using " + secret)
	client := &mockClient{callToolFunc: func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) { return nil, cause }}
	registry := NewToolRegistry()
	registry.Register("remote", client, []*mcpsdk.Tool{makeTool("probe")})
	entry := config.MCPServerEntry{ID: "remote", Token: secret, Env: map[string]string{"DEBUG": "1", "FLAG": "on"}}
	pool := NewClientPool([]config.MCPServerEntry{entry}, registry)
	pool.statuses["remote"] = &clientStatus{entry: entry, client: client, state: "connected"}
	router := NewProxyRouter(registry)
	router.pool = pool
	_, err := router.Handle(context.Background(), callReq("probe"))
	if !errors.Is(err, cause) {
		t.Fatal("redaction lost the upstream error chain")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("returned error exposes bearer credential")
	}
	if !strings.Contains(err.Error(), "connection failed on port 1 using [redacted]") {
		t.Fatalf("short env values mangled error: %s", err)
	}
}
