package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func waitSSEPhase[T any](t *testing.T, ctx context.Context, phase <-chan T) T {
	t.Helper()
	select {
	case value := <-phase:
		return value
	case <-ctx.Done():
		t.Fatalf("SSE phase did not complete: %v", ctx.Err())
		var zero T
		return zero
	}
}

func TestOrdinarySSEProductionPoolSurvivesHandshakeReconnectAndShutdown(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, token := range []string{"", "ordinary-test-bearer"} {
			name := "standalone"
			if shared {
				name = "daemon"
			}
			if token != "" {
				name += "/static-token"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var calls, initializes atomic.Int32
				upstream := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "alpha", Version: "test"}, nil)
				upstream.AddTool(&mcpsdk.Tool{Name: "alpha_probe", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
					calls.Add(1)
					return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
				})
				handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return upstream }, nil)
				streams := make(chan chan struct{}, 8)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					want := ""
					if token != "" {
						want = "Bearer " + token
					}
					if r.Header.Get("Authorization") != want {
						t.Error("ordinary static authorization header changed")
						http.Error(w, "bad auth", 401)
						return
					}
					if r.Method == http.MethodGet {
						done := make(chan struct{})
						streams <- done
						defer close(done)
					}
					if r.Method == http.MethodPost {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							return
						}
						_ = r.Body.Close()
						r.Body = io.NopCloser(bytes.NewReader(body))
						var rpc struct {
							Method string `json:"method"`
						}
						_ = json.Unmarshal(body, &rpc)
						if rpc.Method == "initialize" {
							initializes.Add(1)
						}
					}
					handler.ServeHTTP(w, r)
				}))
				defer server.Close()
				entry := config.MCPServerEntry{ID: "alpha", Transport: "sse", URL: server.URL, Token: token, AllowUnconfinedRemote: true}
				registry := NewToolRegistry()
				pool := NewClientPool([]config.MCPServerEntry{entry}, registry)
				closeOwner := pool.Shutdown
				if shared {
					owner, err := NewSharedUpstreams([]config.MCPServerEntry{entry}, daemonTestRoots(t), true)
					if err != nil {
						t.Fatal(err)
					}
					pool = owner.pool
					registry = owner.registry
					closeOwner = owner.Close
					defer closeOwner()
					if err := owner.Start(ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					defer closeOwner()
					if err := pool.Start(ctx); err != nil {
						t.Fatal(err)
					}
				}
				first := waitSSEPhase(t, ctx, streams)
				router := NewProxyRouter(registry)
				call := func() {
					t.Helper()
					result, err := router.Handle(ctx, ToolCall{ToolName: "alpha_probe", Args: map[string]any{}})
					if err != nil || result == nil || result.IsError {
						t.Fatalf("ordinary SSE call failed: result=%+v error=%v", result, err)
					}
				}
				call()
				registered, ok := registry.Lookup("alpha_probe")
				if !ok {
					t.Fatal("ordinary SSE inventory unavailable")
				}
				if err := registered.Client.Close(); err != nil {
					t.Fatal(err)
				}
				waitSSEPhase(t, ctx, first)
				call()
				second := waitSSEPhase(t, ctx, streams)
				if calls.Load() != 2 || initializes.Load() != 2 {
					t.Fatalf("call/reconnect not observed: calls=%d initialize=%d", calls.Load(), initializes.Load())
				}
				select {
				case <-second:
					t.Fatal("reconnected stream ended before owner shutdown")
				default:
				}
				closeOwner()
				waitSSEPhase(t, ctx, second)
			})
		}
	}
}

// Signal only after the client has consumed upstream bytes, so cancellation
// cannot race ahead of the lifetime observer when probing a stalled endpoint.
type observedSSETransport struct {
	base http.RoundTripper
	read chan struct{}
}

func (o *observedSSETransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := o.base.RoundTrip(r)
	if err == nil && r.Method == http.MethodGet {
		response.Body = &observedSSEBody{ReadCloser: response.Body, read: o.read}
	}
	return response, err
}

type observedSSEBody struct {
	io.ReadCloser
	read chan struct{}
	once sync.Once
}

func (o *observedSSEBody) Read(p []byte) (int, error) {
	n, err := o.ReadCloser.Read(p)
	if n > 0 {
		o.once.Do(func() { close(o.read) })
	}
	return n, err
}

func TestSSEProductionHandshakeCancellationAndFailureCloseGET(t *testing.T) {
	for _, service := range []bool{false, true} {
		for _, phase := range []string{"headers", "endpoint", "keepalive", "partial-lf", "partial-crlf", "initialize", "initialize-error"} {
			name := "ordinary/" + phase
			if service {
				name = "service/" + phase
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				watchdog, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				ctx, cancel := context.WithCancel(watchdog)
				defer cancel()
				observed, getClosed, consumed := make(chan struct{}), make(chan struct{}), make(chan struct{})
				upstream := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "alpha", Version: "test"}, nil)
				handler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return upstream }, nil)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						defer close(getClosed)
						if phase == "initialize" || phase == "initialize-error" {
							handler.ServeHTTP(w, r)
							return
						}
						if phase != "headers" {
							w.Header().Set("Content-Type", "text/event-stream")
							data := ": endpoint has not arrived\n\n"
							switch phase {
							case "keepalive":
								data = "event: keepalive\ndata: {}\n\n"
							case "partial-lf":
								data = "event: endpoint\ndata: /messages\n"
							case "partial-crlf":
								data = "event: endpoint\r\ndata: /messages\r\n\r"
							}
							_, _ = io.WriteString(w, data)
							w.(http.Flusher).Flush()
						}
						close(observed)
						<-r.Context().Done()
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return
					}
					_ = r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(body))
					var rpc struct{ Method string }
					_ = json.Unmarshal(body, &rpc)
					if rpc.Method == "initialize" {
						close(observed)
						if phase == "initialize-error" {
							http.Error(w, "initialize rejected", http.StatusBadRequest)
						} else {
							<-r.Context().Done()
						}
						return
					}
					handler.ServeHTTP(w, r)
				}))
				entry := config.MCPServerEntry{ID: "alpha", Transport: "sse", URL: server.URL}
				pool := NewClientPool([]config.MCPServerEntry{entry}, NewToolRegistry())
				defer func() { cancel(); pool.Shutdown(); server.Close() }()
				pool.remoteHTTPClientFactory = func(entry config.MCPServerEntry) (func(map[string]string, int) *http.Client, error) {
					build := ordinarySSEHTTPClient
					if service {
						var err error
						build, err = serviceHTTPClientFactoryForToken(entry.URL, "test-service-token", true)
						if err != nil {
							return nil, err
						}
					}
					return func(headers map[string]string, seconds int) *http.Client {
						client := build(headers, seconds)
						client.Transport = &observedSSETransport{base: client.Transport, read: consumed}
						return client
					}, nil
				}
				result := make(chan error, 1)
				go func() {
					client, err := pool.connect(ctx, entry)
					if client != nil {
						_ = client.Close()
					}
					result <- err
				}()
				waitSSEPhase(t, watchdog, observed)
				if phase != "headers" && phase != "initialize" && phase != "initialize-error" {
					waitSSEPhase(t, watchdog, consumed)
				}
				if phase != "initialize-error" {
					cancel()
				}
				if err := waitSSEPhase(t, watchdog, result); err == nil {
					t.Fatal("incomplete or rejected handshake unexpectedly succeeded")
				}
				// No elapsed-time assertion: the failed/canceled SDK handshake
				// must actually terminate its long-lived GET before completion.
				waitSSEPhase(t, watchdog, getClosed)
			})
		}
	}
}
