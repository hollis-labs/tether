package mcpadapter

import (
	"context"
	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"reflect"
	"strings"
	"testing"
)

func TestCatalogOriginExpectations(t *testing.T) {
	enabled := map[string]bool{"registry": true, "torque": true, "disabled": false, "ops": true}
	external := map[string]bool{"ops": true}
	for _, tc := range []struct {
		name           string
		explicit, want []string
	}{
		{"default", nil, []string{"registry", "torque"}},
		{"explicit", []string{"missing", "registry", "registry"}, []string{"missing", "registry"}},
		{"empty", []string{}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot mcpgateway.Snapshot
			applyOriginExpectations(&snapshot, tc.explicit, enabled, external, map[string]string{"ops": "standalone gateway :18190"})
			if !reflect.DeepEqual(snapshot.ExpectedOrigins, tc.want) {
				t.Fatalf("%v want %v", snapshot.ExpectedOrigins, tc.want)
			}
			if len(snapshot.External) != 1 || snapshot.External[0].Status != "external" || snapshot.External[0].ServedAt != "standalone gateway :18190" {
				t.Fatalf("%+v", snapshot.External)
			}
		})
	}
}

func TestDirectoryToolDescriptions(t *testing.T) {
	a, _ := newRegistryAdapterWithScopes(t, []string{ScopeRegistryWrite})
	s := gomcp.NewServer("test", "0.0.1")
	a.registerRegistryTools(s)
	a.registerBindingsTools(s)
	a.registerScopedBindingsTools(s)
	c := connectInMemory(t, s)
	listed, err := c.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tool := range listed.Tools {
		if strings.HasPrefix(tool.Name, "tether_registry_") {
			count++
			if !strings.HasPrefix(tool.Description, directoryDescriptionPrefix) {
				t.Errorf("%s lacks directory prefix: %s", tool.Name, tool.Description)
			}
		}
	}
	if count < 16 {
		t.Fatalf("only checked %d directory tools", count)
	}
}

func TestGatewaySnapshotKeepsCatalogExpectationsOutsideGrant(t *testing.T) {
	a := newTestAdapter(t)
	a.svc.Catalog = &config.Catalog{MCPServerEnabled: map[string]bool{"registry": true, "ops": true}, MCPServerExternal: map[string]bool{"ops": true}, MCPServerServedAt: map[string]string{"ops": "standalone gateway :18190"}}
	// The caller has no registry tools/origin at all, as after grant filtering.
	gateway := a.gatewayService(NewToolRegistry(), nil, mcpgateway.Selection{Mode: mcpgateway.Flat}, nil)
	st := gateway.Status("")
	if st.Complete || !reflect.DeepEqual(st.MissingExpected, []string{"registry"}) || len(st.External) != 1 {
		t.Fatalf("%+v", st)
	}
	a.svc.Catalog.Global.MCP.ExpectedOrigins = []string{}
	if st = gateway.Status(""); !st.Complete || len(st.MissingExpected) != 0 {
		t.Fatalf("explicit empty expectations: %+v", st)
	}
	a.svc.Catalog.Global.MCP.ExpectedOrigins = []string{"removed"}
	if st = gateway.Status(""); st.Complete || !reflect.DeepEqual(st.MissingExpected, []string{"removed"}) {
		t.Fatalf("explicit removed origin: %+v", st)
	}
}
