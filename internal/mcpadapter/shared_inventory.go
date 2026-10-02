package mcpadapter

import (
	"reflect"
	"sort"

	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// visibleInventory is the accepted, currently available target inventory of
// one immutable view. It excludes both hidden origins and profile-denied tools.
func visibleInventory(gateway *mcpgateway.Service) map[string]*mcpsdk.Tool {
	snapshot := gateway.Policy.Eligible(gateway.Snapshot())
	unavailable := map[string]bool{}
	for _, origin := range snapshot.Origins {
		if origin.Status != "connected" {
			unavailable[origin.ID] = true
		}
	}
	tools := map[string]*mcpsdk.Tool{}
	for _, entry := range snapshot.Entries {
		if !unavailable[entry.Origin] {
			tools[entry.Tool.Name] = entry.Tool
		}
	}
	return tools
}

// reconcileVisible registers only eligible targets. SDK notifications therefore
// cannot leak a change to an ungranted/denied declaration. Updated names are
// replaced atomically and never removed first.
func reconcileVisible(live *liveProxyCatalog, previous, next map[string]*mcpsdk.Tool) {
	removed := []string{}
	changed := []*mcpsdk.Tool{}
	for name := range previous {
		if _, ok := next[name]; !ok {
			removed = append(removed, name)
		}
	}
	for name, tool := range next {
		if !reflect.DeepEqual(previous[name], tool) {
			changed = append(changed, tool)
		}
	}
	sort.Strings(removed)
	sort.Slice(changed, func(i, j int) bool { return changed[i].Name < changed[j].Name })
	if len(removed) > 0 {
		live.server.SDKServer().RemoveTools(removed...)
	}
	live.addProxyTools(changed...)
}
