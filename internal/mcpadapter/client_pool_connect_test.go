package mcpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

// TestConnectTransports covers ClientPool.connect's transport switch. It is the
// only place entry.Transport is interpreted — every other read is pass-through
// into status reporting — so an unhandled transport surfaces here or nowhere.
//
// Unlike mark3labs' two-phase NewStreamableHttpClient+Start (construct, then
// separately dial), the official SDK's Client.Connect performs the full
// connect+handshake as one synchronous call -- so, unlike before, the http
// cases need a real listener to connect to, not just a URL.
func TestConnectTransports(t *testing.T) {
	pool := NewClientPool(nil, NewToolRegistry())
	ctx := context.Background()

	newUpstream := func() *httptest.Server {
		upstream := gomcp.NewServer("upstream", "test")
		registerTestTool(upstream, "probe")
		handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream.SDKServer() }, nil)
		return httptest.NewServer(handler)
	}

	t.Run("http connects to a real listener", func(t *testing.T) {
		srv := newUpstream()
		defer srv.Close()
		c, err := pool.connect(ctx, config.MCPServerEntry{
			ID:        "tangent",
			Transport: "http",
			URL:       srv.URL,
		})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if c == nil {
			t.Fatal("connect returned a nil client")
		}
		if err := c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	t.Run("http with a bearer token", func(t *testing.T) {
		srv := newUpstream()
		defer srv.Close()
		c, err := pool.connect(ctx, config.MCPServerEntry{
			ID:        "tangent",
			Transport: "http",
			URL:       srv.URL,
			Token:     "tok",
		})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	t.Run("http requires url", func(t *testing.T) {
		_, err := pool.connect(ctx, config.MCPServerEntry{ID: "x", Transport: "http"})
		if err == nil {
			t.Fatal("expected an error for http with no url")
		}
		if !strings.Contains(err.Error(), "http transport requires url") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("unknown transport names every supported one", func(t *testing.T) {
		_, err := pool.connect(ctx, config.MCPServerEntry{ID: "x", Transport: "carrier-pigeon"})
		if err == nil {
			t.Fatal("expected an error for an unknown transport")
		}
		for _, want := range []string{"carrier-pigeon", "stdio", "sse", "http"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}
