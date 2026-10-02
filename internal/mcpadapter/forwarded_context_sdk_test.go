package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServiceContextThroughSDKReconnect(t *testing.T) {
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) { testServiceContextThroughSDKReconnect(t, transport) })
	}
}

func testServiceContextThroughSDKReconnect(t *testing.T, transport string) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "service.token")
	if err := os.WriteFile(path, []byte("upstream-sdk-service"), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := gomcp.NewServer("upstream", "test")
	registerTestTool(upstream, "probe")
	var sdkHandler http.Handler = mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream.SDKServer() }, nil)
	if transport == "sse" {
		sdkHandler = mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return upstream.SDKServer() }, nil)
	}
	type observed struct{ method, actor, auth, session, expected string }
	var mu sync.Mutex
	var seen []observed
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		expected := ""
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			var rpc struct {
				Method string `json:"method"`
				Params struct {
					Meta map[string]any `json:"_meta"`
				} `json:"params"`
			}
			_ = json.Unmarshal(body, &rpc)
			expected, _ = rpc.Params.Meta["test_expected_session"].(string)
			if rpc.Method != "" {
				method = rpc.Method
			}
		}
		mu.Lock()
		seen = append(seen, observed{method, r.Header.Get("X-Forwarded-User-Id"), r.Header.Get("Authorization"), r.Header.Get("X-Tether-Session-Id"), expected})
		mu.Unlock()
		sdkHandler.ServeHTTP(w, r)
	}))
	defer server.Close()
	private, factory, err := daemonHTTPPolicies([]config.MCPServerEntry{{ID: "remote", Transport: transport, URL: server.URL, ProxyServiceTokenFile: path}})
	if err != nil {
		t.Fatal(err)
	}
	pool := NewClientPool(private, NewToolRegistry())
	pool.remoteHTTPClientFactory = factory
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A verified caller can cause initialization/reconnect, but those RPCs must
	// not gain the actual tool call's actor marker.
	p := identity.Principal{ID: "principal:A", Kind: "session", SessionID: "A"}
	caller := callcontext.WithSnapshot(identity.WithPrincipal(ctx, p), callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: p.ID, PrincipalKind: p.Kind, SessionID: p.SessionID})
	client, err := pool.connect(caller, private[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	call := func(callCtx context.Context) {
		t.Helper()
		snapshot, _ := callcontext.FromContext(callCtx)
		result, err := client.CallTool(callCtx, &mcpsdk.CallToolParams{Name: "probe", Arguments: map[string]any{}, Meta: mcpsdk.Meta{"test_expected_session": snapshot.SessionID}})
		if err != nil || result.IsError {
			t.Fatalf("SDK tool call failed: %v", err)
		}
	}
	call(caller)
	// Force go-mcp reconnect on the next tool call, under the same caller context.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	call(caller)
	call(ctx)
	var calls sync.WaitGroup
	for i := range 12 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			session := fmt.Sprintf("parallel-%d", i)
			principal := identity.Principal{ID: "principal:" + session, Kind: "session", SessionID: session}
			parallelCtx := callcontext.WithSnapshot(identity.WithPrincipal(ctx, principal), callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: principal.ID, PrincipalKind: principal.Kind, SessionID: session})
			call(parallelCtx)
		}()
	}
	calls.Wait()
	mu.Lock()
	defer mu.Unlock()
	actors := 0
	initializes := 0
	for _, request := range seen {
		if request.auth != "Bearer upstream-sdk-service" {
			t.Fatal("SDK request lacked service authentication")
		}
		if request.method == "initialize" {
			initializes++
		}
		if request.actor != "" {
			if request.method != "tools/call" || request.actor != "session:"+request.session || request.session != request.expected {
				t.Fatalf("actor leaked to %s", request.method)
			}
			actors++
		}
	}
	if actors != 14 || initializes < 2 {
		t.Fatalf("did not exercise attributed concurrent calls and reconnect: actors=%d initializes=%d", actors, initializes)
	}
}
