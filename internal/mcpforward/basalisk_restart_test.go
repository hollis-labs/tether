package mcpforward

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Basalisk's child-process client initializes with 2025-03-26 and retains its
// ready flag across RPC errors. It needs the same connection and tools/call to
// recover transparently, without the child process or handshake restarting.
func TestBasalisk20250326ToolCallAcrossActualServerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var calls atomic.Int32
	newHandler := func() http.Handler {
		server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
		server.AddTool(&mcp.Tool{Name: "write", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	}
	server := httptest.NewServer(newHandler())
	defer server.Close()
	address := server.Listener.Addr().String()
	left, right := mcp.NewInMemoryTransports()
	connection, err := left.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client.New("tcp:"+strings.TrimPrefix(server.URL, "http://")), client.MCPOptions{}, right)
	}()
	send := func(raw string) error {
		message, err := jsonrpc.DecodeMessage([]byte(raw))
		if err != nil {
			return err
		}
		return connection.Write(ctx, message)
	}
	exchange := func(raw string) (*jsonrpc.Response, error) {
		if err := send(raw); err != nil {
			return nil, err
		}
		for {
			message, err := connection.Read(ctx)
			if err != nil {
				return nil, err
			}
			if reply, ok := message.(*jsonrpc.Response); ok {
				return reply, reply.Error
			}
		}
	}
	initialized, err := exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"basalisk-shape","version":"1"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(initialized.Result, &result); err != nil || result.ProtocolVersion != "2025-03-26" {
		t.Fatalf("negotiated %q: %v", result.ProtocolVersion, err)
	}
	if err := send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := exchange(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write","arguments":{}}}`); err != nil {
		t.Fatal(err)
	}
	server.CloseClientConnections()
	server.Close()
	completed := make(chan error, 1)
	expectedID, _ := jsonrpc.MakeID(float64(3))
	go func() {
		reply, err := exchange(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write","arguments":{}}}`)
		if err == nil && (reply.ID != expectedID || !strings.Contains(string(reply.Result), "ok")) {
			err = fmt.Errorf("unexpected replacement response: %+v", reply)
		}
		completed <- err
	}()
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
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if calls.Load() != 2 {
		t.Fatalf("tool executed %d times; want one before and one after restart", calls.Load())
	}
	connection.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Log("2025-03-26 connection kept alive; tools/call recovered transparently and executed once after restart")
}
