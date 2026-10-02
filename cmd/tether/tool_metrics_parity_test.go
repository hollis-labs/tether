package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const metricsFixtureEnv = "TETHER_TEST_TOOL_METRICS_DAEMON"

func TestToolMetricsMCPFixture(t *testing.T) {
	addr := os.Getenv(metricsFixtureEnv)
	if addr == "" {
		return
	}
	a := mcpadapter.NewWithDaemon(&app.Service{Catalog: &config.Catalog{}}, client.New(addr, client.WithToken("")), "", nil)
	if err := a.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type metricParitySource struct {
	api.EventsStore
	err error
}

func (s metricParitySource) QueryToolCallMetrics(context.Context, telemetry.MetricsQuery) ([]telemetry.MetricGroup, error) {
	return []telemetry.MetricGroup{{Tool: "read", Upstream: "alpha", Outcome: "denied", Calls: 3, ArgsBytes: 9, Duration: telemetry.Histogram{Count: 3, SumMs: 25, Buckets: []telemetry.Bucket{{Count: 3}}}}}, s.err
}
func TestToolMetrics_MCPHTTPCLIParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "")
	t.Setenv("HOLLIS_OTEL_DISABLED", "1")
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	oldCatalog := catalogPath
	t.Cleanup(func() { catalogPath = oldCatalog })
	for _, failure := range []string{"", "internal", "invalid"} {
		t.Run(failure, func(t *testing.T) {
			source := metricParitySource{}
			if failure == "internal" {
				source.err = errors.New("metrics unavailable")
			}
			srv := httptest.NewServer(api.NewHandler(api.Deps{EventsStore: source}))
			defer srv.Close()
			catalogPath = writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
			cmd := newToolMetricsCmd()
			flags := []string{"--json"}
			path := "/events/tool-metrics"
			args := map[string]any{}
			if failure == "invalid" {
				flags = append(flags, "--since=invalid")
				path += "?since=invalid"
				args["since"] = "invalid"
			}
			if err := cmd.ParseFlags(flags); err != nil {
				t.Fatal(err)
			}
			var cliErr error
			cliJSON := captureStdout(t, func() { cliErr = cmd.RunE(cmd, nil) })
			rr := httptest.NewRecorder()
			srv.Config.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(exe, "-test.run=^TestToolMetricsMCPFixture$")
			command.Env = append(os.Environ(), metricsFixtureEnv+"=tcp:"+strings.TrimPrefix(srv.URL, "http://"), "GORACE=atexit_sleep_ms=0")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "metrics-parity", Version: "test"}, nil).Connect(ctx, &mcpsdk.CommandTransport{Command: command}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()
			result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_metrics", Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			text := result.Content[0].(*mcpsdk.TextContent).Text
			if failure != "" {
				code := "internal_error"
				if failure == "invalid" {
					code = "invalid_request"
				}
				var httpErr struct{ Error struct{ Code string } }
				var mcpErr struct{ Code string }
				if err := json.Unmarshal(rr.Body.Bytes(), &httpErr); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(text), &mcpErr); err != nil {
					t.Fatal(err)
				}
				if !result.IsError || httpErr.Error.Code != code || mcpErr.Code != code || cliErr == nil || !strings.Contains(cliErr.Error(), "("+code+")") {
					t.Fatalf("parity HTTP=%s MCP=%s CLI=%v", rr.Body.String(), text, cliErr)
				}
				return
			}
			var httpOut, mcpOut, cliOut telemetry.MetricsResponse
			for _, value := range []struct {
				raw []byte
				dst *telemetry.MetricsResponse
			}{{rr.Body.Bytes(), &httpOut}, {[]byte(text), &mcpOut}, {[]byte(cliJSON), &cliOut}} {
				if err := json.Unmarshal(value.raw, value.dst); err != nil {
					t.Fatal(err)
				}
			}
			if cliErr != nil || result.IsError || !reflect.DeepEqual(httpOut, mcpOut) || !reflect.DeepEqual(httpOut, cliOut) {
				t.Fatalf("HTTP %+v MCP %+v CLI %+v/%v", httpOut, mcpOut, cliOut, cliErr)
			}
		})
	}
}
