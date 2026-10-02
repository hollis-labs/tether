package mcpforward

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestForwardDaemonAdmittedToolsAndResult(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	daemon := mcp.NewServer(&mcp.Implementation{Name: "stub", Version: "1"}, nil)
	daemon.AddTool(&mcp.Tool{Name: "allowed_echo", Title: "Exact title", Description: "admitted", InputSchema: map[string]any{"type": "object"}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if req.Params.Meta["canary"] != "metadata" {
			t.Error("metadata lost")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}, StructuredContent: map[string]any{"context": "verified"}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer session-canary" {
			t.Error("wrong forwarding credential")
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	left, right := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("session-canary")), client.MCPOptions{}, right)
	}()
	consumer, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, left, nil)
	if err != nil {
		select {
		case relayErr := <-done:
			t.Fatalf("initialize: %v; relay: %v", err, relayErr)
		default:
			t.Fatal(err)
		}
	}
	tools, err := consumer.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "allowed_echo" || tools.Tools[0].Title != "Exact title" || !tools.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("admitted inventory lost: %+v %v", tools, err)
	}
	result, err := consumer.CallTool(ctx, &mcp.CallToolParams{Name: "allowed_echo", Arguments: map[string]any{"message": "hello"}, Meta: mcp.Meta{"canary": "metadata"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if string(raw) != `{"context":"verified"}` {
		t.Fatalf("result lost: %s", raw)
	}
	_ = consumer.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("forwarder did not close")
	}
}

func TestForwardDaemonPreservesProgressAndInventoryNotifications(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	daemon := mcp.NewServer(&mcp.Implementation{Name: "stub", Version: "1"}, nil)
	daemon.AddTool(&mcp.Tool{Name: "progress", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := req.Params.GetProgressToken()
		if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: 1, Total: 2}); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
	})
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	left, right := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("session")), client.MCPOptions{}, right)
	}()
	progress := make(chan any, 1)
	changed := make(chan bool, 1)
	consumer, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) { progress <- r.Params.ProgressToken },
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case changed <- true:
			default:
			}
		},
	}).Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	params := &mcp.CallToolParams{Name: "progress", Arguments: map[string]any{}, Meta: mcp.Meta{"progressToken": "agent-progress"}}
	if _, err := consumer.CallTool(ctx, params); err != nil {
		t.Fatal(err)
	}
	select {
	case token := <-progress:
		if token != "agent-progress" {
			t.Fatalf("progress token changed: %v", token)
		}
	case <-ctx.Done():
		t.Fatal("progress was lost")
	}
	daemon.AddTool(&mcp.Tool{Name: "added", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("inventory notification was lost")
	}
	tools, err := consumer.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("new admitted inventory not relayed: %+v %v", tools, err)
	}
	_ = consumer.Close()
	<-done
}

func TestForwardDaemonDownReturnsTypedErrorWithoutFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := os.MkdirTemp("/var/tmp", "fwd-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	left, right := mcp.NewInMemoryTransports()
	connection, err := left.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client.New("unix:"+filepath.Join(root, "missing.sock"), client.WithToken("session-canary")), client.MCPOptions{}, right)
	}()
	request, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"agent","version":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, request); err != nil {
		t.Fatal(err)
	}
	message, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	response, ok := message.(*jsonrpc.Response)
	if !ok || response.ID != request.(*jsonrpc.Request).ID {
		t.Fatalf("initialization error ID lost: %+v", message)
	}
	var protocol *jsonrpc.Error
	if !errors.As(response.Error, &protocol) || protocol.Code != -32001 || !strings.Contains(protocol.Message, "daemon_unreachable") {
		t.Fatalf("missing typed daemon failure: %v", response.Error)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("daemon-down succeeded")
		}
	case <-ctx.Done():
		t.Fatal("daemon-down tried local fallback or blocked")
	}
}
