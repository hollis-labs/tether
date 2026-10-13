package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/hollis-labs/tether/internal/config"
)

// catalogState guards the swappable catalog. A *config.Catalog is never
// mutated after it is loaded, so a reader that took the pointer keeps a
// consistent view while a reload swaps in the next one.
type catalogState struct {
	mu       sync.RWMutex
	reloadMu sync.Mutex
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
	next := *fresh
	next.Global = old.Global
	next.Paths = old.Paths

	s.catalogState.mu.Lock()
	s.Catalog = &next
	s.catalogState.mu.Unlock()
	report.Reloaded = true
	return &report, nil
}
