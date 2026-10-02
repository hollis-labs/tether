package mcpadapter

import "fmt"

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

func (p *ClientPool) unavailableError(id string, client upstreamClient) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.statuses[id]
	if s == nil || s.state != "connected" || s.client != client {
		state := "unavailable"
		if s != nil {
			state = s.state
		}
		return fmt.Errorf("upstream %q unavailable (%s); request was not sent; inspect tether_gateway_status", id, state)
	}
	return nil
}
