package app

import (
	"sync"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/eventstamp"
)

// catalogSeats gives the seat stamp stage the catalog's agents: each agent's
// cell (its own, else the nearest one up its extends chain) and the agent id
// behind a registry URN. It follows catalog reloads; the index is rebuilt when
// the catalog pointer changes. The catalog is in memory, so lookups never touch
// the database (the stage is TxSafe).
type catalogSeats struct {
	current func() *config.Catalog

	mu    sync.Mutex
	built *config.Catalog
	cells map[string]string
	urns  map[string]string
}

var _ eventstamp.Seats = (*catalogSeats)(nil)

func (c *catalogSeats) Cell(agentID string) string {
	cells, _ := c.index()
	return cells[agentID]
}

func (c *catalogSeats) AgentForURN(urn string) string {
	if urn == "" {
		return ""
	}
	_, urns := c.index()
	return urns[urn]
}

// follow points the directory at the live catalog once the service exists.
func (c *catalogSeats) follow(current func() *config.Catalog) {
	c.mu.Lock()
	c.current = current
	c.mu.Unlock()
}

func (c *catalogSeats) index() (map[string]string, map[string]string) {
	c.mu.Lock()
	current := c.current
	c.mu.Unlock()
	var cat *config.Catalog
	if current != nil {
		cat = current()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cat == c.built && c.cells != nil {
		return c.cells, c.urns
	}
	c.built = cat
	c.cells, c.urns = seatIndex(cat)
	return c.cells, c.urns
}

// seatIndex maps agent id to cell, and URN (an agent's urn:, and the
// msg://agent/agent-mux/<id> address mail uses) to agent id.
func seatIndex(cat *config.Catalog) (map[string]string, map[string]string) {
	cells, urns := map[string]string{}, map[string]string{}
	if cat == nil {
		return cells, urns
	}
	for id, a := range cat.Agents {
		cell := a.Cell
		seen := map[string]bool{id: true}
		for parent := a.Extends; cell == "" && parent != "" && !seen[parent]; {
			seen[parent] = true
			p, ok := cat.Agents[parent]
			if !ok {
				break
			}
			cell, parent = p.Cell, p.Extends
		}
		if cell != "" {
			cells[id] = cell
		}
		urns["msg://agent/agent-mux/"+id] = id
	}
	// A declared identity wins over the id-shaped address.
	for id, a := range cat.Agents {
		if a.URN != "" {
			urns[a.URN] = id
		}
	}
	return cells, urns
}
