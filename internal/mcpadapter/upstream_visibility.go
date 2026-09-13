package mcpadapter

import (
	"fmt"

	mcpclient "github.com/mark3labs/mcp-go/client"
)

func unavailableServers(statuses []ServerStatus) []ServerStatus {
	out := make([]ServerStatus, 0)
	for _, s := range statuses {
		if s.Status != "connected" {
			out = append(out, s)
		}
	}
	return out
}

func (a *Adapter) upstreamStatus() []ServerStatus {
	if a.upstreams == nil {
		return nil
	}
	return a.upstreams.StatusSummary()
}

func unavailableIDs(statuses []ServerStatus) map[string]bool {
	out := make(map[string]bool)
	for _, s := range unavailableServers(statuses) {
		out[s.ID] = true
	}
	return out
}

func addAvailability(payload map[string]any, statuses []ServerStatus) {
	if statuses == nil {
		return
	}
	absent := unavailableServers(statuses)
	payload["complete"] = len(absent) == 0
	payload["unavailable_servers"] = absent
	if len(absent) > 0 {
		payload["availability_hint"] = "Discovery is incomplete: unavailable upstreams were excluded. Empty results do not establish that those products have no matching tool. Inspect mux_catalog_list_mcp_servers for exit, retry and stderr diagnostics."
	}
}

func (p *ClientPool) unavailableError(id string, client mcpclient.MCPClient) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.statuses[id]
	if s == nil || s.state != "connected" || s.client != client {
		state := "unavailable"
		if s != nil {
			state = s.state
		}
		return fmt.Errorf("upstream %q unavailable (%s); request was not sent; inspect mux_catalog_list_mcp_servers", id, state)
	}
	return nil
}
