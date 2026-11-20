package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGatewayRelayCommandReadsRunTokenHandoffWithoutInheritedEnvironment(t *testing.T) {
	t.Setenv("TETHER_RUN_TOKEN", "")
	tokenPath, runPath := filepath.Join(t.TempDir(), "jwt"), filepath.Join(t.TempDir(), "run")
	if err := tokenfile.Write(tokenPath, "synthetic-jwt"); err != nil {
		t.Fatal(err)
	}
	if err := tokenfile.Write(runPath, "synthetic-run"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "gateway", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "transcript_probe", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "probe succeeded"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-jwt" || r.Header.Get("X-Run-Token") != "synthetic-run" {
			t.Error("protected handoff did not reach gateway")
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"transcript_probe","arguments":{}}}
`)
	var output, diagnostics bytes.Buffer
	cmd := newMCPHTTPRelayCommand()
	cmd.SetIn(input)
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostics)
	cmd.SetArgs([]string{"--url", gateway.URL, "--token-file", tokenPath, "--run-token-file", runPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(output.String(), "transcript_probe") || !strings.Contains(output.String(), "probe succeeded") {
		t.Fatal("gateway discovery/call did not complete")
	}
	for _, secret := range []string{"synthetic-jwt", "synthetic-run"} {
		if strings.Contains(output.String()+diagnostics.String(), secret) {
			t.Fatal("relay printed credential")
		}
	}
}
