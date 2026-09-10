package mcpadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// TestConnectTransports covers ClientPool.connect's transport switch. It is the
// only place entry.Transport is interpreted — every other read is pass-through
// into status reporting — so an unhandled transport surfaces here or nowhere.
//
// The http cases construct a real streamable client but never reach the
// network: NewStreamableHttpClient does not dial, and Start opens no connection
// unless continuous listening is enabled.
func TestConnectTransports(t *testing.T) {
	pool := &ClientPool{}
	ctx := context.Background()

	t.Run("http builds a client without dialing", func(t *testing.T) {
		c, err := pool.connect(ctx, config.MCPServerEntry{
			ID:        "tangent",
			Transport: "http",
			URL:       "http://127.0.0.1:7842/mcp",
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
		c, err := pool.connect(ctx, config.MCPServerEntry{
			ID:        "tangent",
			Transport: "http",
			URL:       "http://127.0.0.1:7842/mcp",
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
