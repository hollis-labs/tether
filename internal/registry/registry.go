package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"

	"github.com/hollis-labs/tether/internal/config"
)

// DefaultCatalogRelPath is the catalog root relative to the user home
// directory. It mirrors the path Tether's config loader reads today.
const DefaultCatalogRelPath = ".tether/catalog"

// Registry is Tether's registry-resolution layer. It owns a
// go-agent-launch FileBackedRegistrar (ingested from a local catalog
// root) fronted by a DegradingRegistrar so registry-down degrades to a
// last-known-good cache rather than hard-failing a launch (D1).
//
// A Registry is safe for concurrent use after construction: every method
// dispatches through the underlying concurrency-safe registrar. It is
// constructed with Open or OpenAt and is never mutated afterwards.
type Registry struct {
	root      string
	registrar *agentlaunch.DegradingRegistrar
	report    agentlaunch.IngestReport
}

// Options configures Registry construction.
type Options struct {
	// CatalogRoot is the catalog directory to ingest. When empty, Open
	// falls back to <home>/.tether/catalog. Path expansion (~) is applied.
	CatalogRoot string

	// CachePath, when set, makes the degrading registrar's last-known-good
	// cache durable across process restarts by mirroring it to this local
	// JSON file. When empty the cache is in-memory only. This is the only
	// optional I/O the registry performs and it is strictly local (D1).
	CachePath string

	// OnDegrade, when set, is invoked by the degrading registrar whenever
	// it crosses a degradation boundary (healthy<->degraded). It is the
	// observability seam; the registry never hard-codes a logger.
	OnDegrade func(reason string)
}

// Open constructs a Registry from the default catalog root
// (<home>/.tether/catalog) with an in-memory degradation cache. It is the
// zero-config entry point; callers needing a non-default root or durable
// cache use OpenAt.
func Open() (*Registry, error) {
	return OpenAt(Options{})
}

// OpenAt constructs a Registry from opts. It builds a FileBackedRegistrar
// over the catalog root, ingests it once, wraps it in a DegradingRegistrar,
// and returns the Registry plus a queryable IngestReport.
//
// Construction is fully offline: the only I/O is local filesystem reads of
// the catalog root (and, if Options.CachePath is set, a best-effort read
// of the cache file). An unreadable catalog root is a hard error; a
// malformed individual catalog file is not — it is recorded in the
// IngestReport (see Report) and never aborts construction.
func OpenAt(opts Options) (*Registry, error) {
	root, err := resolveCatalogRoot(opts.CatalogRoot)
	if err != nil {
		return nil, err
	}

	// PerKindRegistrationValidator enforces that each ingested record's
	// metadata is coherent for its kind. The file-backed registrar stamps
	// the published schema-version/interface itself, so this is a
	// belt-and-braces check rather than a gate on caller input.
	inner := agentlaunch.NewInMemoryRegistrar(
		agentlaunch.WithRecordValidator(agentlaunch.PerKindRegistrationValidator),
	)
	fbr := agentlaunch.NewFileBackedRegistrar(root, agentlaunch.WithRegistrar(inner))

	report, err := fbr.IngestCatalog()
	if err != nil {
		return nil, fmt.Errorf("registry: ingest catalog %s: %w", root, err)
	}

	var cacheOpts []agentlaunch.CacheOption
	if opts.CachePath != "" {
		cacheOpts = append(cacheOpts, agentlaunch.WithCachePersistence(config.Expand(opts.CachePath)))
	}
	cache := agentlaunch.NewLastKnownGoodCache(cacheOpts...)

	var degOpts []agentlaunch.DegradingOption
	if opts.OnDegrade != nil {
		degOpts = append(degOpts, agentlaunch.WithDegradeHook(opts.OnDegrade))
	}
	degrading := agentlaunch.NewDegradingRegistrar(fbr.Registrar(), cache, degOpts...)

	return newRegistry(root, degrading, report), nil
}

// newRegistry assembles a Registry around an already-built degrading
// registrar and ingest report. It is the shared tail of OpenAt and the
// in-package test seam (see openWithRegistrar) so both paths produce an
// identically-shaped Registry.
func newRegistry(root string, degrading *agentlaunch.DegradingRegistrar, report agentlaunch.IngestReport) *Registry {
	return &Registry{root: root, registrar: degrading, report: report}
}

// resolveCatalogRoot turns a caller-supplied catalog root into an absolute
// path, falling back to <home>/.tether/catalog when empty.
func resolveCatalogRoot(raw string) (string, error) {
	if raw != "" {
		return config.Expand(raw), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("registry: resolve home directory: %w", err)
	}
	return filepath.Join(home, DefaultCatalogRelPath), nil
}

// Root returns the absolute catalog root the registry was ingested from.
func (r *Registry) Root() string { return r.root }

// Report returns the IngestReport produced when the catalog was ingested.
// It is the reconciliation surface: record counts per kind/subdir, any
// IngestSkip outcomes (projects/ and sandbox-profiles/ have no registry
// kind and are reported as unmapped-directory skips — that is expected),
// and any per-file IngestError. Callers should inspect it to surface
// catalog health.
func (r *Registry) Report() agentlaunch.IngestReport { return r.report }

// Status returns the current degradation posture of the underlying
// registrar. It is purely observational; a degraded posture means query
// reads are being served from the last-known-good cache.
func (r *Registry) Status() agentlaunch.DegradeStatus {
	return r.registrar.Status()
}

// Health dispatches a registry health envelope and returns the
// HealthPayload. The degrading registrar never reports a hard failure
// here — at worst it reports HealthStatusDegraded.
func (r *Registry) Health() (*agentlaunch.HealthPayload, error) {
	resp, err := r.registrar.Handle(agentlaunch.RegistryEnvelope{
		Version:    agentlaunch.RegistryEnvelopeVersionV1,
		Resolution: agentlaunch.RegistryResolutionLocalFirst,
		Operation:  agentlaunch.RegistryOperationHealth,
		Registrar:  r.registrarDescriptor(),
		Health:     &agentlaunch.HealthPayload{},
	})
	if err != nil {
		return nil, fmt.Errorf("registry: health: %w", err)
	}
	return resp.Health, nil
}

// registrarDescriptor builds the RegistryRegistrar descriptor stamped on
// every envelope this package sends. The file-backed mode requires a
// non-empty FileRoot, which is the catalog root.
func (r *Registry) registrarDescriptor() agentlaunch.RegistryRegistrar {
	return agentlaunch.RegistryRegistrar{
		Mode:     agentlaunch.RegistrarModeFileBacked,
		FileRoot: r.root,
	}
}

// queryOne runs a single-record query for (kind, name) against the
// degrading registrar and returns the matching RegistrationRecord. An
// unresolvable id is a hard error: a launch input that cannot be resolved
// must fail loudly, never fall back silently.
//
// notFound is the precise sentinel returned (wrapped) when the kind/name
// pair has no record; callers branch on it with errors.Is.
func (r *Registry) queryOne(kind agentlaunch.RegistryKind, name string, notFound error) (agentlaunch.RegistrationRecord, error) {
	if name == "" {
		return agentlaunch.RegistrationRecord{}, fmt.Errorf("%w: empty id", notFound)
	}
	resp, err := r.registrar.Handle(agentlaunch.RegistryEnvelope{
		Version:    agentlaunch.RegistryEnvelopeVersionV1,
		Resolution: agentlaunch.RegistryResolutionLocalFirst,
		Operation:  agentlaunch.RegistryOperationQuery,
		Registrar:  r.registrarDescriptor(),
		Query: &agentlaunch.QueryPayload{
			Kinds: []agentlaunch.RegistryKind{kind},
			Name:  name,
		},
	})
	if err != nil {
		// The degrading registrar only ever surfaces ErrRegistryCacheMiss
		// here (directory down AND nothing cached). Treat that as an
		// unresolved id for this kind/name pair so the caller sees a
		// precise, kind-aware error.
		if errors.Is(err, agentlaunch.ErrRegistryCacheMiss) {
			return agentlaunch.RegistrationRecord{}, fmt.Errorf("%w: %q (registry degraded, no cache entry): %w", notFound, name, err)
		}
		return agentlaunch.RegistrationRecord{}, fmt.Errorf("registry: query %s %q: %w", kind, name, err)
	}
	switch len(resp.Records) {
	case 0:
		return agentlaunch.RegistrationRecord{}, fmt.Errorf("%w: %q", notFound, name)
	case 1:
		return resp.Records[0], nil
	default:
		// A name is unique within a kind in the file-backed registrar
		// (the catalog id is the record name); more than one match means a
		// catalog integrity problem worth surfacing rather than guessing.
		return agentlaunch.RegistrationRecord{}, fmt.Errorf("registry: %s %q resolves to %d records, expected 1", kind, name, len(resp.Records))
	}
}
