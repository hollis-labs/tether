package mcpforward

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRunTokenGateway401ReconnectsReadWithoutRepostingMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	daemon := mcp.NewServer(&mcp.Implementation{Name: "gateway", Version: "1"}, nil)
	daemon.AddTool(&mcp.Tool{Name: "mutate", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
	var initialized, reads, calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Run-Token") != "synthetic-run" || r.Header.Get("Authorization") != "Bearer synthetic-bearer" {
			t.Error("gateway credential handoff lost")
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &msg)
			switch msg.Method {
			case "initialize":
				initialized.Add(1)
			case "tools/list":
				if reads.Add(1) == 1 {
					w.WriteHeader(401)
					return
				}
			case "tools/call":
				calls.Add(1)
				w.WriteHeader(401)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	relay := &daemonSession{client: client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("synthetic-bearer")), opts: client.MCPOptions{RunToken: "synthetic-run"}}
	defer relay.close()
	if _, err := relay.invokeRead(ctx, func(s *mcp.ClientSession) (mcp.Result, error) { return s.ListTools(ctx, nil) }); err != nil {
		t.Fatal("read did not reconnect after 401", err)
	}
	if reads.Load() != 2 || initialized.Load() != 2 {
		t.Fatal("read did not replace its rejected session", reads.Load(), initialized.Load())
	}
	if _, err := relay.callTool(ctx, &mcp.CallToolParams{Name: "mutate"}); err == nil {
		t.Fatal("mutation refusal hidden")
	}
	if calls.Load() != 1 {
		t.Fatal("mutation replayed", calls.Load())
	}
	if _, err := relay.invokeRead(ctx, func(s *mcp.ClientSession) (mcp.Result, error) { return s.ListTools(ctx, nil) }); err != nil {
		t.Fatal("read after mutation did not reconnect", err)
	}
	if initialized.Load() != 3 || calls.Load() != 1 {
		t.Fatal("invalid session retirement")
	}
}
