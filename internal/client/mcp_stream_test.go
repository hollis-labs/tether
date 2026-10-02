package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func notificationRetryFixture(t *testing.T) (*mcp.ClientSession, chan struct{}, <-chan struct{}, *atomic.Int32, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	end := make(chan struct{})
	resumed := make(chan struct{})
	var endOnce sync.Once
	var getCount, dispatches atomic.Int32
	daemon := mcp.NewServer(&mcp.Implementation{Name: "retry-fixture", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	daemon.AddTool(&mcp.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}}, func(callCtx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		endOnce.Do(func() { close(end) })
		select {
		case <-resumed:
		case <-callCtx.Done():
			return nil, callCtx.Err()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "real result"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if getCount.Add(1) == 1 {
				_, _ = io.WriteString(w, "id: notification-cursor\ndata:\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-end:
				case <-r.Context().Done():
				}
				return
			}
			if r.Header.Get("Last-Event-ID") != "notification-cursor" {
				t.Error("retry did not resume notification cursor", r.Header.Get("Last-Event-ID"))
			}
			close(resumed)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.Method == http.MethodPost {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &msg)
			if msg.Method == "tools/call" {
				dispatches.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	session, err := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithToken("session")).ConnectMCP(ctx, MCPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = session.Close() })
	return session, end, resumed, &dispatches, ctx
}

func TestMCPNotificationRetryPreservesInflightCall(t *testing.T) {
	session, _, _, dispatches, ctx := notificationRetryFixture(t)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "slow"})
	if err != nil {
		t.Fatal("notification GET retry killed in-flight POST", err)
	}
	if len(result.Content) != 1 || result.Content[0].(*mcp.TextContent).Text != "real result" {
		t.Fatal("lost actual tool result", result)
	}
	if dispatches.Load() != 1 {
		t.Fatal("tools/call replayed", dispatches.Load())
	}
}

func TestMCPNotificationRetryAllowsNextCall(t *testing.T) {
	session, end, resumed, _, ctx := notificationRetryFixture(t)
	failed := make(chan error, 1)
	go func() { failed <- session.Wait() }()
	close(end)
	select {
	case <-resumed:
	case err := <-failed:
		t.Fatal("notification GET end retired session", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := session.Ping(ctx, nil); err != nil {
		t.Fatal("next call after GET resumed", err)
	}
}
