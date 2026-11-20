package mcpforward

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Stop the real listener and all connected streams, then bind a fresh server
// with empty session state at the same address under a still-connected client.
func TestForwarderRecoversAcrossActualServerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var calls atomic.Int32
	newHandler := func() http.Handler {
		server := mcp.NewServer(&mcp.Implementation{Name: "restart", Version: "1"}, nil)
		server.AddTool(&mcp.Tool{Name: "write", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	}
	server := httptest.NewServer(newHandler())
	address := server.Listener.Addr().String()
	defer server.Close()
	left, right := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client.New("tcp:"+strings.TrimPrefix(server.URL, "http://")), client.MCPOptions{}, right)
	}()
	consumer, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	if _, err = consumer.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	server.CloseClientConnections()
	server.Close()
	result := make(chan error, 1)
	go func() { _, err := consumer.CallTool(ctx, &mcp.CallToolParams{Name: "write"}); result <- err }()
	// Keep the endpoint down through the first reconnect, exercising backoff.
	time.Sleep(1200 * time.Millisecond)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	replacement := httptest.NewUnstartedServer(newHandler())
	replacement.Listener.Close()
	replacement.Listener = listener
	replacement.Start()
	defer replacement.Close()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if calls.Load() != 1 {
		t.Fatalf("write executed %d times", calls.Load())
	}
	t.Log("connected client recovered and dispatched tool exactly once after real TCP server restart")
	consumer.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestForwarderReinitializesExplicitSessionLoss(t *testing.T) {
	for _, fixture := range []struct {
		name, body string
		status     int
	}{
		{"http-initialize-required", `{"error":{"code":"mcp_initialize_required"}}`, 400},
		{"expired", `{"error":{"code":"session_expired"}}`, 404},
		{"sdk-404", "session not found", 404},
		{"rpc-initialize-required", `{"jsonrpc":"2.0","id":REPLACE,"error":{"code":-32000,"message":"mcp_initialize_required"}}`, 200},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			upstream := mcp.NewServer(&mcp.Implementation{Name: "loss", Version: "1"}, nil)
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, nil)
			var rejected atomic.Bool
			var inits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					raw, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
					var msg struct {
						Method string
						ID     json.RawMessage
					}
					json.Unmarshal(raw, &msg)
					if msg.Method == "initialize" {
						inits.Add(1)
					}
					if msg.Method == "tools/list" && rejected.CompareAndSwap(false, true) {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(fixture.status)
						io.WriteString(w, strings.ReplaceAll(fixture.body, "REPLACE", string(msg.ID)))
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			consumer, finish := forwardConsumer(t, server.URL)
			defer finish()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := consumer.ListTools(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if inits.Load() != 2 {
				t.Fatalf("initialize count %d", inits.Load())
			}
		})
	}
}
