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

// TestRemoteClient_CallToolPreservesMeta locks in the reason remoteClient
// bypasses go-mcp/client's own CallTool wrapper: that wrapper has no _meta
// parameter, and proxy.go's provenance/trace-context injection depends on
// _meta reaching the upstream on every forwarded call. Regressing this would
// not fail loudly -- the call would still succeed, just silently carrying no
// provenance or trace context -- so it needs its own direct assertion rather
// than resting on TestConnectTransports' "did it connect" coverage.
func TestRemoteClient_CallToolPreservesMeta(t *testing.T) {
	var gotMeta map[string]any
	upstream := gomcp.NewServer("upstream", "test")
	upstream.RegisterTool(gomcp.Tool{
		Name:         "echo_meta",
		Description:  "captures the call's protocol _meta",
		InputSchema:  gomcp.EmptyObjectSchema(),
		ReadOnlyHint: true,
		Handler: func(ctx context.Context, _ map[string]any) (any, error) {
			gotMeta = gomcp.MetaFromContext(ctx)
			return "ok", nil
		},
	})
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream.SDKServer() }, nil))
	defer srv.Close()

	pool := NewClientPool(nil, NewToolRegistry())
	client, err := pool.connect(context.Background(), config.MCPServerEntry{
		ID:        "meta-probe",
		Transport: "http",
		URL:       srv.URL,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	res, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "echo_meta",
		Meta: mcpsdk.Meta{"tether.provenance": map[string]any{"session_id": "sess-1"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("echo_meta returned an error result: %+v", res)
	}
	if gotMeta == nil {
		t.Fatal("upstream received no _meta; remoteClient.CallTool dropped it")
	}
	prov, _ := gotMeta["tether.provenance"].(map[string]any)
	if prov["session_id"] != "sess-1" {
		t.Errorf("_meta reached upstream as %v, want tether.provenance.session_id=sess-1", gotMeta)
	}
}
