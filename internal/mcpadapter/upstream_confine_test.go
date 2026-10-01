package mcpadapter

import (
	"context"
	"fmt"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestConfinedRemoteUpstreams_ReportedAndNotDialed(t *testing.T) {
	entries := []config.MCPServerEntry{{ID: "http", Transport: "http"}, {ID: "sse", Transport: "sse"}}
	pool := NewClientPool(entries, NewToolRegistry())
	pool.confineRemote = true
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		t.Error("excluded remote dialed")
		return nil, fmt.Errorf("unexpected dial")
	})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	status := pool.StatusSummary()
	if len(status) != 2 {
		t.Fatalf("status=%v", status)
	}
	for _, s := range status {
		if s.Status != "excluded" || !strings.Contains(s.Error, "cannot be confined locally") || s.RestartAttempts != 0 {
			t.Fatalf("status=%+v", s)
		}
	}
}

func TestConfineUpstreamTransport_ExplicitOperatorOptIn(t *testing.T) {
	for _, transport := range []string{"http", "sse", "stdio"} {
		entry := config.MCPServerEntry{ID: "test", Transport: transport}
		if err := confineUpstreamTransport(entry, false); err != nil {
			t.Fatal("ordinary proxy changed", err)
		}
		entry.AllowUnconfinedRemote = true
		if err := confineUpstreamTransport(entry, true); err != nil {
			t.Fatal("operator opt-in refused", err)
		}
		entry.AllowUnconfinedRemote = false
		if transport == "stdio" && confineUpstreamTransport(entry, true) != nil {
			t.Fatal("local upstream refused")
		}
	}
}

func TestConfinedRemoteUpstreams_CatalogOptInConnects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	upstream := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "test"}, nil)
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream }, nil))
	defer srv.Close()
	catalog := t.TempDir()
	if err := os.Mkdir(filepath.Join(catalog, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf("id: test\ntransport: http\nurl: %s\nallow_unconfined_remote: true\n", srv.URL)
	if err := os.WriteFile(filepath.Join(catalog, "mcp-servers", "test.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := config.LoadMCPServers(catalog)
	if err != nil {
		t.Fatal(err)
	}
	pool := NewClientPool(entries, NewToolRegistry())
	pool.confineRemote = true
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	status := pool.StatusSummary()
	if len(status) != 1 || status[0].Status != "connected" {
		t.Fatalf("opt-in remote status: %+v", status)
	}
}
