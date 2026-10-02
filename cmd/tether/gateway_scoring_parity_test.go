package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const scoringCLICatalogEnv = "TETHER_TEST_SCORING_CLI_CATALOG"

// Execute the actual CLI command with a disposable empty upstream catalog.
func TestScoringCLIStdioFixture(t *testing.T) {
	catalog := os.Getenv(scoringCLICatalogEnv)
	if catalog == "" {
		return
	}
	rootCmd.SetArgs([]string{"--catalog", catalog, "mcp", "--proxy", "--discovery-mode", "search", "--servers="})
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestGatewayScoring_CLIStdioHTTPParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CODEX_") || strings.HasPrefix(key, "TETHER_") {
			t.Setenv(key, "")
		}
	}
	// A local disposable daemon stub keeps context lookups/telemetry off the host.
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/context" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer daemon.Close()
	catalog := writeEventsTestCatalog(t, strings.TrimPrefix(daemon.URL, "http://"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := mcpadapter.NewSharedUpstreams(nil, mcpadapter.DaemonProtectedRoots{Catalog: catalog, Run: t.TempDir(), State: t.TempDir(), CatalogConfig: &config.Catalog{}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	caller := identity.WithPrincipal(ctx, identity.Principal{ID: "test", Kind: "operator"})
	adapter, err := mcpadapter.NewVerifiedAdapter(caller, &app.Service{Catalog: &config.Catalog{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := runtime.NewGatewayView(caller, adapter, mcpadapter.ProxyOptions{ServerFilter: []string{}, ModeInputs: mcpgateway.ModeInputs{Explicit: []mcpgateway.Selector{{Value: "search", Source: "test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	httpServer := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return view.Server }, nil))
	defer httpServer.Close()
	httpClient, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "scoring-http", Version: "test"}, nil).Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = httpClient.Close() }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(exe, "-test.run=^TestScoringCLIStdioFixture$")
	command.Env = append(os.Environ(), scoringCLICatalogEnv+"="+catalog, "GORACE=atexit_sleep_ms=0")
	cliClient, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "scoring-cli", Version: "test"}, nil).Connect(ctx, &mcpsdk.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cliClient.Close() }()
	for _, query := range []string{"inbox", "   "} {
		var results []*mcpsdk.CallToolResult
		for _, session := range []*mcpsdk.ClientSession{httpClient, cliClient} {
			result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_search", Arguments: map[string]any{"query": query}})
			if err != nil {
				t.Fatal(err)
			}
			results = append(results, result)
		}
		var bodies []map[string]any
		for _, result := range results {
			if len(result.Content) != 1 {
				t.Fatalf("content=%+v", result)
			}
			text, ok := result.Content[0].(*mcpsdk.TextContent)
			if !ok {
				t.Fatalf("content=%T", result.Content[0])
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, body)
		}
		if !reflect.DeepEqual(bodies[0], bodies[1]) || results[0].IsError != results[1].IsError {
			t.Fatalf("query=%q HTTP=%+v/%v CLI-stdio=%+v/%v", query, bodies[0], results[0].IsError, bodies[1], results[1].IsError)
		}
		if strings.TrimSpace(query) == "" {
			if !results[0].IsError || bodies[0]["code"] != "invalid_request" {
				t.Fatalf("invalid query=%+v", bodies[0])
			}
			continue
		}
		expected, err := view.Service.Search(mcpgateway.Request{Query: query})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(expected)
		var normalized map[string]any
		if err := json.Unmarshal(raw, &normalized); err != nil {
			t.Fatal(err)
		}
		if results[0].IsError || expected.Returned == 0 || !reflect.DeepEqual(bodies[0], normalized) {
			t.Fatalf("query=%q HTTP=%+v service=%+v", query, bodies[0], expected)
		}
	}
}
