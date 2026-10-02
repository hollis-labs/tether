package mcpforward

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestForwardDaemonErrorMatrixNeverRepostsCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int64
	}{
		{"expiry-json", 404, `{"error":{"code":"mcp_session_not_found"}}`, -32002},
		{"expiry-sdk", 404, "session not found", -32002},
		{"route", 404, "no route", -32002},
		{"server-error", 500, "failed", -32005},
		{"unavailable", 503, "not ready", -32005},
		{"rate-limit", 429, "busy", -32005},
		{"unauthorized", 401, "unauthorized", -32002},
		{"forbidden", 403, "forbidden", -32002},
		{"json-reset", 0, "", -32001},
		{"sse-no-id", 0, "", -32001},
		{"sse-primed", 0, "", -32001},
		{"sse-partial-reset", 0, "", -32001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := mcp.NewServer(&mcp.Implementation{Name: "matrix", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
			var mu sync.Mutex
			seen := map[string][]string{}
			var probed bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.Header.Get("Last-Event-ID") == "call-cursor" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				if r.Method == http.MethodPost {
					body, _ := io.ReadAll(r.Body)
					_ = r.Body.Close()
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					var request struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
						Params struct {
							Name      string         `json:"name"`
							Arguments map[string]any `json:"arguments"`
						} `json:"params"`
					}
					_ = json.Unmarshal(body, &request)
					if request.Method == "ping" {
						mu.Lock()
						probed = true
						mu.Unlock()
					}
					if request.Method == "tools/call" {
						mu.Lock()
						hasProbe := probed
						mu.Unlock()
						if !hasProbe {
							t.Error("tools/call dispatched before ping")
						}
						if request.Params.Name != "mutate" || request.Params.Arguments["message"] != "effect" {
							t.Error("call content changed", string(body))
						}
						mu.Lock()
						seen[string(request.ID)] = append(seen[string(request.ID)], string(body))
						mu.Unlock()
						if tc.status != 0 {
							w.WriteHeader(tc.status)
							_, _ = io.WriteString(w, tc.body)
							return
						}
						if tc.name == "sse-no-id" || tc.name == "sse-primed" {
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(200)
							if tc.name == "sse-primed" {
								_, _ = io.WriteString(w, "id: call-cursor\ndata:\n\n")
							} else {
								_, _ = io.WriteString(w, "data:\n\n")
							}
							w.(http.Flusher).Flush()
							return
						}
						conn, rw, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						if tc.name == "json-reset" {
							_, _ = fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"jsonrpc\":")
						} else {
							_, _ = fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\ndata: {\"jsonrpc\":")
						}
						_ = rw.Flush()
						_ = conn.Close()
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			consumer, finish := forwardConsumer(t, server.URL)
			defer finish()
			_, err := consumer.CallTool(t.Context(), &mcp.CallToolParams{Name: "mutate", Arguments: map[string]any{"message": "effect"}})
			var protocol *jsonrpc.Error
			if !errors.As(err, &protocol) || protocol.Code != tc.code {
				t.Errorf("caller category want %d: %v", tc.code, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("request identities=%v", seen)
			}
			for id, bodies := range seen {
				if len(bodies) != 1 {
					t.Fatalf("tools/call %s was dispatched %d times: %v", id, len(bodies), bodies)
				}
			}
		})
	}
}
