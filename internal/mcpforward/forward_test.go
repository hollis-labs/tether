package mcpforward

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestForwardDaemonRecoversExpiredView(t *testing.T) {
	daemon := mcp.NewServer(&mcp.Implementation{Name: "idle", Version: "1"}, nil)
	var calls, initializations atomic.Int32
	daemon.AddTool(&mcp.Tool{Name: "mutate", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	var expired atomic.Bool
	var original atomic.Pointer[string]
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("Mcp-Session-Id"); id != "" {
			original.CompareAndSwap(nil, &id)
			if expired.Load() && id == *original.Load() {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "session not found")
				return
			}
		}

		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			var request struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &request)
			if request.Method == "initialize" {
				initializations.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	consumer, finish := forwardConsumer(t, server.URL)
	defer finish()
	if _, err := consumer.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	expired.Store(true)
	if _, err := consumer.CallTool(context.Background(), &mcp.CallToolParams{Name: "mutate"}); err != nil {
		t.Fatal("expired view never recovered", err)
	}
	if calls.Load() != 1 || initializations.Load() != 2 {
		t.Fatalf("executions=%d initializations=%d", calls.Load(), initializations.Load())
	}
}

func TestForwardDaemonDoesNotReplayUncertainMutation(t *testing.T) {
	for _, failure := range []string{"lost-response", "missing-session"} {
		t.Run(failure, func(t *testing.T) {
			daemon := mcp.NewServer(&mcp.Implementation{Name: "uncertain", Version: "1"}, nil)
			var calls atomic.Int32
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					body, _ := io.ReadAll(r.Body)
					_ = r.Body.Close()
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					var request struct {
						Method string `json:"method"`
					}
					_ = json.Unmarshal(body, &request)
					if request.Method == "tools/call" {
						// Model a completed side effect whose HTTP response was lost.
						calls.Add(1)
						if failure == "missing-session" {
							w.WriteHeader(http.StatusNotFound)
							_, _ = w.Write([]byte(`{"error":{"code":"mcp_session_not_found"}}`))
							return
						}
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			consumer, finish := forwardConsumer(t, server.URL)
			defer finish()
			_, err := consumer.CallTool(context.Background(), &mcp.CallToolParams{Name: "mutate"})
			var protocol *jsonrpc.Error
			wantCode := int64(-32001)
			if failure == "missing-session" {
				wantCode = -32002
			}
			if !errors.As(err, &protocol) || protocol.Code != wantCode {
				t.Fatalf("lost response: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("uncertain mutation replayed: %d", calls.Load())
			}
		})
	}
}

func TestRelayErrorCancellationAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		err     error
		code    int64
		message string
	}{
		{context.Canceled, -32003, "canceled"},
		{errors.New("request terminated without response"), -32001, "daemon_unreachable"},
		{errors.New("standalone SSE stream: exceeded 0 retries without progress (session ID: example)"), -32001, "daemon_unreachable"},
		{errors.New("standalone SSE request failed (session ID: example): daemon unreachable: connection refused"), -32001, "daemon_unreachable"},
		{&net.OpError{Op: "read", Net: "unix", Err: context.DeadlineExceeded}, -32001, "daemon_unreachable"},
		{errors.New("sending tools/list: 404 Not Found"), -32002, "daemon_mcp_unavailable"},
		{errors.New("standalone SSE request failed: 401 Unauthorized"), -32002, "daemon_mcp_unavailable"},
		{errors.New("503 Service Unavailable: mcp_stopping"), -32002, "daemon_mcp_unavailable"},
		{&jsonrpc.Error{Code: -32603, Message: "connection refused"}, -32603, "connection refused"},
		{context.DeadlineExceeded, -32004, "timeout"},
	} {
		var protocol *jsonrpc.Error
		if !errors.As(relayError(tc.err), &protocol) || protocol.Code != tc.code || !strings.Contains(protocol.Message, tc.message) || strings.Contains(protocol.Message, "credential rejected") {
			t.Fatalf("wrong category: %+v", protocol)
		}
	}
}

func forwardConsumer(t *testing.T, endpoint string) (*mcp.ClientSession, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	left, right := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client.New("tcp:"+strings.TrimPrefix(endpoint, "http://"), client.WithToken("session")), client.MCPOptions{}, right)
	}()
	consumer, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, left, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return consumer, func() { _ = consumer.Close(); cancel(); <-done }
}

func TestForwardDaemonAllowsColdInitialization(t *testing.T) {
	daemon := mcp.NewServer(&mcp.Implementation{Name: "cold", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			time.Sleep(2200 * time.Millisecond)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	consumer, finish := forwardConsumer(t, server.URL)
	defer finish()
	if err := consumer.Ping(context.Background(), nil); err != nil {
		t.Fatal("cold initialization failed", err)
	}
}

func TestForwardDaemonUnknown404DoesNotRecover(t *testing.T) {
	daemon := mcp.NewServer(&mcp.Implementation{Name: "unknown-404", Version: "1"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
	var initializes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			var request struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &request)
			if request.Method == "initialize" {
				initializes.Add(1)
			}
			if request.Method == "tools/list" {
				http.NotFound(w, r)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	consumer, finish := forwardConsumer(t, server.URL)
	defer finish()
	if _, err := consumer.ListTools(context.Background(), nil); err == nil {
		t.Fatal("unknown 404 swallowed")
	}
	if initializes.Load() != 1 {
		t.Fatalf("unknown 404 triggered recovery: %d initializes", initializes.Load())
	}
	// A later operation may initialize a fresh view once the SDK marks its
	// previous connection closed; each unknown 404 still surfaces to its caller.
	if _, err := consumer.ListTools(context.Background(), nil); err == nil {
		t.Fatal("second unknown 404 swallowed")
	}
}
