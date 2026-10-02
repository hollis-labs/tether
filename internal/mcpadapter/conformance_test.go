package mcpadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type conformanceClient struct {
	*mockClient
	instructions string
}

func (c *conformanceClient) InitializeResult() *mcpsdk.InitializeResult {
	return &mcpsdk.InitializeResult{Instructions: c.instructions}
}

func TestConformanceObservedInstructionsWithoutToolInventoryOrStartupFailure(t *testing.T) {
	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{{ID: "alpha", Transport: "http"}}, registry)
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		return &conformanceClient{instructions: strings.Repeat("界", 2049), mockClient: &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{}, nil
		}}}, nil
	})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	findings, _ := registry.NameDiagnostics()
	if len(findings) != 1 || findings[0].Origin != "alpha" || findings[0].Code != "instructions_length" || findings[0].Name != "" {
		t.Fatalf("initialize observation without tools: %+v", findings)
	}
	registry.RecordInstructions("alpha", 2048)
	findings, _ = registry.NameDiagnostics()
	if len(findings) != 0 {
		t.Fatalf("stale initialization finding: %+v", findings)
	}
}
