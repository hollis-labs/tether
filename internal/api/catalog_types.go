package api

import "github.com/hollis-labs/tether/internal/config"

// CatalogLoader returns a fresh catalog view for each call. The default
// daemon-side implementation re-reads the filesystem, so edits to the
// catalog YAML files are picked up without a daemon restart. Tests pass
// a stub that returns a pre-built *config.Catalog (or an error).
//
// Load returning an error is surfaced as an internal_error envelope —
// handlers never panic on a broken catalog.
type CatalogLoader interface {
	Load() (*config.Catalog, error)
}

// ListProjectsResponse is the GET /catalog/projects body. Projects is
// always present (non-nil even when empty) to keep decoders simple.
type ListProjectsResponse struct {
	Projects []config.Project `json:"projects"`
}

type ListAgentsResponse struct {
	Agents []config.Agent `json:"agents"`
}

type ListProvidersResponse struct {
	Providers []config.Provider `json:"providers"`
}

type ListLaunchesResponse struct {
	Launches []config.Launch `json:"launches"`
}
