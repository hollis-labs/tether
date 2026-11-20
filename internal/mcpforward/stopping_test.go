package mcpforward

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestForwarderRecoversGracefulStopBeforeToolDispatch(t *testing.T) {
	var rejected atomic.Bool
	var stopInit atomic.Int32
	var executions, attempts atomic.Int32
	upstream := mcp.NewServer(&mcp.Implementation{Name: "stopping", Version: "1"}, nil)
	upstream.AddTool(&mcp.Tool{Name: "write", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		executions.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			var msg struct{ Method string }
			json.Unmarshal(raw, &msg)
			stopping := false
			if msg.Method == "tools/call" {
				attempts.Add(1)
				stopping = rejected.CompareAndSwap(false, true)
			}
			if msg.Method == "initialize" && rejected.Load() {
				stopping = stopInit.Add(1) == 1
			}
			if stopping {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":{"code":"mcp_stopping"}}`)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	consumer, finish := forwardConsumer(t, server.URL)
	defer finish()
	result, err := consumer.CallTool(context.Background(), &mcp.CallToolParams{Name: "write"})
	if err != nil || result == nil {
		t.Fatalf("graceful-stop tool recovery: %v", err)
	}
	if attempts.Load() != 2 || executions.Load() != 1 || stopInit.Load() != 2 {
		t.Fatalf("attempts=%d executions=%d init=%d", attempts.Load(), executions.Load(), stopInit.Load())
	}
}
