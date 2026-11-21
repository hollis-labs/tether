package mcpadapter

import (
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"sort"
)

// Expectations describe the catalog, independently of the caller's grants.
func applyOriginExpectations(snapshot *mcpgateway.Snapshot, explicit []string, enabled, external map[string]bool, servedAt map[string]string) {
	snapshot.ExpectedOrigins = []string{}
	snapshot.External = []mcpgateway.ExternalOrigin{}
	expected := map[string]bool{}
	for id, on := range enabled {
		if !on {
			continue
		}
		if external[id] {
			location := servedAt[id]
			if location == "" {
				location = "standalone gateway (outside Tether; consult the service runbook)"
			}
			snapshot.External = append(snapshot.External, mcpgateway.ExternalOrigin{ID: id, Status: "external", ServedAt: location})
		} else if explicit == nil {
			expected[id] = true
		}
	}
	for _, id := range explicit {
		expected[id] = true
	}
	for id := range expected {
		snapshot.ExpectedOrigins = append(snapshot.ExpectedOrigins, id)
	}
	sort.Strings(snapshot.ExpectedOrigins)
	sort.Slice(snapshot.External, func(i, j int) bool { return snapshot.External[i].ID < snapshot.External[j].ID })
}
