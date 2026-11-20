package mcptransport

import (
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"testing"
)

func TestSelectedOriginsExcludeExternalAfterSelection(t *testing.T) {
	origins, err := selectedUpstreamOrigins(admission{
		catalog: &config.Catalog{MCPServerEnabled: map[string]bool{"local": true, "shared-services": true}, MCPServerExternal: map[string]bool{"shared-services": true}},
		options: mcpadapter.ProxyOptions{ServerFilter: []string{"local", "shared-services"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(origins) != 1 || origins[0] != "local" {
		t.Fatalf("built-in start origins = %v", origins)
	}
}
