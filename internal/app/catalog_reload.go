package app

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/hollis-labs/tether/internal/config"
)

// catalogState guards the swappable catalog. A *config.Catalog is never
// mutated after it is loaded, so a reader that took the pointer keeps a
// consistent view while a reload swaps in the next one.
type catalogState struct {
	mu         sync.RWMutex
	reloadMu   sync.Mutex
	generation uint64
}

// CurrentCatalog returns the catalog launches resolve against right now.
// Read it through here, not the Catalog field, so a reload is seen.
func (s *Service) CurrentCatalog() *config.Catalog {
	s.catalogState.mu.RLock()
	defer s.catalogState.mu.RUnlock()
	return s.Catalog
}

func (s *Service) cat() *config.Catalog { return s.CurrentCatalog() }

// ReloadCatalog re-reads the catalog from disk, validates it, and only then
// swaps it in, so agents, projects, providers, launches and sandbox profiles
// edited on disk apply to new launches without a daemon restart. A catalog
// that does not load or validate changes nothing and is returned as an error.
//
// Running sessions keep what they started with. global.yaml settings the
// daemon read at start (listener, identity, AI providers, MCP gateway, modules)
// are not swapped: the running values are kept and the report names the edit
// under RestartRequired.
func (s *Service) ReloadCatalog(_ context.Context) (*config.ReloadReport, error) {
	s.catalogState.reloadMu.Lock()
	defer s.catalogState.reloadMu.Unlock()

	old := s.CurrentCatalog()
	if old == nil || s.CatalogRoot == "" {
		return nil, fmt.Errorf("catalog reload needs a daemon started from a catalog")
	}
	fresh, err := config.LoadLayered(s.CatalogRoot)
	if err != nil {
		return nil, fmt.Errorf("reload refused, catalog does not load (nothing changed): %w", err)
	}
	if err := fresh.ValidateMCPGrants(); err != nil {
		return nil, fmt.Errorf("reload refused, MCP grants (nothing changed): %w", err)
	}
	if err := fresh.Validate(); err != nil {
		return nil, fmt.Errorf("reload refused, catalog invalid (nothing changed): %w", err)
	}
	report := config.DiffCatalogs(old, fresh)
	if config.GlobalDiffers(old, fresh) {
		report.RestartRequired = append(report.RestartRequired,
			"global.yaml changed: daemon, identity, AI, MCP gateway and module settings are read at start and still need a restart")
	}
	// A server the catalog now enables that the running daemon never started is not live after this reload:
	// its upstream proxy opens at daemon start. Say so (CW-20261024-0097); operator MCP views meanwhile
	// list it as pending_restart instead of failing.
	var added []string
	for id, enabled := range fresh.MCPServerEnabled {
		if was, known := old.MCPServerEnabled[id]; enabled && (!known || !was) {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	for _, id := range added {
		report.RestartRequired = append(report.RestartRequired,
			"mcp-servers/"+id+".yaml enables an MCP upstream the running daemon has not started; it stays pending_restart in tether_gateway_status until the daemon is reloaded")
	}
	next := *fresh
	next.Global = old.Global
	next.Paths = old.Paths

	s.catalogState.mu.Lock()
	s.Catalog = &next
	s.catalogState.generation++
	s.catalogState.mu.Unlock()
	report.Reloaded = true
	return &report, nil
}

// CatalogGeneration is a process-local reload counter; zero is the initial load.
func (s *Service) CatalogGeneration() uint64 {
	s.catalogState.mu.RLock()
	defer s.catalogState.mu.RUnlock()
	return s.catalogState.generation
}
