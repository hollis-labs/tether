package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

func TestDoctorLiveReportsMetadataWithoutCallingOrRewritingTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_MCP_SERVERS", "alpha")
	upstream := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "alpha", Version: "test"}, &mcpsdk.ServerOptions{Instructions: strings.Repeat("界", 2049)})
	upstream.AddTool(&mcpsdk.Tool{Name: "alpha_disabled", Description: "Disabled: " + strings.Repeat("界", 2049), InputSchema: map[string]any{"type": "object", "properties": map[string]any{"secret-schema": 123}}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		t.Error("doctor called a tool")
		return &mcpsdk.CallToolResult{}, nil
	})
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream }, nil))
	defer srv.Close()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(config.MCPServerEntry{ID: "alpha", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp-servers", "alpha.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	results := checkMCPLiveNames(&config.Catalog{}, root)
	for _, code := range []string{"missing_annotations", "description_length", "disabled_description", "input_schema", "instructions_length"} {
		found := false
		for _, result := range results {
			if result.Name == "mcp-name:alpha:"+code && (strings.Contains(result.Remedy, "tool declaration") || strings.Contains(result.Remedy, "initialization instructions")) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s: %+v", code, results)
		}
	}
}

func TestDoctorLiveReportsAggregateConformanceRemainderCount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_MCP_SERVERS", "alpha")
	upstream := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "alpha", Version: "test"}, nil)
	for i := 0; i < 5; i++ {
		upstream.AddTool(&mcpsdk.Tool{Name: fmt.Sprintf("alpha_%02d", i), Annotations: &mcpsdk.ToolAnnotations{}, InputSchema: map[string]any{"type": "object", "description": strings.Repeat("x", 200*1024)}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			t.Error("doctor called a tool")
			return &mcpsdk.CallToolResult{}, nil
		})
	}
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream }, nil))
	defer srv.Close()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(config.MCPServerEntry{ID: "alpha", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp-servers", "alpha.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	results := checkMCPLiveNames(&config.Catalog{}, root)
	for _, result := range results {
		if result.Name == "mcp-name:alpha:conformance_unexamined" {
			if !strings.Contains(result.Message, "3 tool declarations unexamined") {
				t.Fatalf("remainder count: %+v", result)
			}
			return
		}
	}
	t.Fatalf("missing aggregate remainder finding: %+v", results)
}
