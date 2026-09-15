package launchprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var (
	// ErrProfileNotFound indicates the requested launch profile was not found.
	ErrProfileNotFound = errors.New("launch profile not found")

	// ErrContextNotFound indicates the requested launch context was not found.
	ErrContextNotFound = errors.New("launch context not found")

	// ErrCycle indicates a cycle was detected while following an extends chain.
	ErrCycle = errors.New("extends cycle detected")
)

// Source is the composition-source adapter interface.
// It retrieves launch profiles and launch contexts for resolution.
//
// Tether ships with built-in FileSource and MemorySource implementations,
// allowing offline, standalone operation with zero external runtime dependencies.
type Source interface {
	// GetProfile fetches a LaunchProfile by ID. Returns ErrProfileNotFound if absent.
	GetProfile(ctx context.Context, id string) (*LaunchProfile, error)

	// GetContext fetches a LaunchContext by ID. Returns ErrContextNotFound if absent.
	GetContext(ctx context.Context, id string) (*LaunchContext, error)
}

// MemorySource is an in-memory Source useful for testing and caller-inline definitions.
type MemorySource struct {
	mu       sync.RWMutex
	profiles map[string]*LaunchProfile
	contexts map[string]*LaunchContext
}

// NewMemorySource creates a new initialized MemorySource.
func NewMemorySource() *MemorySource {
	return &MemorySource{
		profiles: make(map[string]*LaunchProfile),
		contexts: make(map[string]*LaunchContext),
	}
}

// AddProfile inserts or replaces a LaunchProfile.
func (m *MemorySource) AddProfile(p *LaunchProfile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.profiles[p.ID] = p
}

// AddContext inserts or replaces a LaunchContext.
func (m *MemorySource) AddContext(c *LaunchContext) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contexts[c.ID] = c
}

func (m *MemorySource) GetProfile(_ context.Context, id string) (*LaunchProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.profiles[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrProfileNotFound, id)
	}
	return p, nil
}

func (m *MemorySource) GetContext(_ context.Context, id string) (*LaunchContext, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.contexts[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrContextNotFound, id)
	}
	return c, nil
}

// FileSource reads profiles and contexts from a directory on the local filesystem.
type FileSource struct {
	RootDir string
}

// NewFileSource creates a FileSource reading from rootDir.
func NewFileSource(rootDir string) *FileSource {
	return &FileSource{RootDir: rootDir}
}

func (f *FileSource) candidateProfilePaths(id string) []string {
	clean := filepath.Clean(id)
	return []string{
		filepath.Join(f.RootDir, clean+".yaml"),
		filepath.Join(f.RootDir, clean+".yml"),
		filepath.Join(f.RootDir, clean+".json"),
		filepath.Join(f.RootDir, "profiles", clean+".yaml"),
		filepath.Join(f.RootDir, "profiles", clean+".yml"),
		filepath.Join(f.RootDir, "launch-profiles", clean+".yaml"),
		filepath.Join(f.RootDir, "agents", clean+".yaml"),
		filepath.Join(f.RootDir, "agents", clean+".yml"),
	}
}

func (f *FileSource) candidateContextPaths(id string) []string {
	clean := filepath.Clean(id)
	return []string{
		filepath.Join(f.RootDir, clean+".yaml"),
		filepath.Join(f.RootDir, clean+".yml"),
		filepath.Join(f.RootDir, clean+".json"),
		filepath.Join(f.RootDir, "projects", clean+".yaml"),
		filepath.Join(f.RootDir, "projects", clean+".yml"),
		filepath.Join(f.RootDir, "contexts", clean+".yaml"),
	}
}

func (f *FileSource) GetProfile(_ context.Context, id string) (*LaunchProfile, error) {
	for _, p := range f.candidateProfilePaths(id) {
		b, err := os.ReadFile(p) //nolint:gosec // G304: catalog/profile-sourced path
		if err != nil {
			continue
		}
		profile, err := parseProfileBytes(b, id)
		if err != nil {
			return nil, fmt.Errorf("parsing profile from %s: %w", p, err)
		}
		return profile, nil
	}
	return nil, fmt.Errorf("%w: %q (under %s)", ErrProfileNotFound, id, f.RootDir)
}

func (f *FileSource) GetContext(_ context.Context, id string) (*LaunchContext, error) {
	for _, p := range f.candidateContextPaths(id) {
		b, err := os.ReadFile(p) //nolint:gosec // G304: catalog/project-sourced path
		if err != nil {
			continue
		}
		var ctx LaunchContext
		if strings.HasSuffix(p, ".json") {
			if err := json.Unmarshal(b, &ctx); err != nil {
				return nil, fmt.Errorf("parsing context json from %s: %w", p, err)
			}
		} else {
			if err := yaml.Unmarshal(b, &ctx); err != nil {
				return nil, fmt.Errorf("parsing context yaml from %s: %w", p, err)
			}
		}
		if ctx.ID == "" {
			ctx.ID = id
		}
		return &ctx, nil
	}
	return nil, fmt.Errorf("%w: %q (under %s)", ErrContextNotFound, id, f.RootDir)
}

// CairnBundleSource reads profiles from a Cairn bundle structure without
// requiring Cairn as a runtime dependency.
type CairnBundleSource struct {
	BundleRoot string
}

// NewCairnBundleSource creates a CairnBundleSource rooted at bundleRoot.
func NewCairnBundleSource(bundleRoot string) *CairnBundleSource {
	return &CairnBundleSource{BundleRoot: bundleRoot}
}

func (c *CairnBundleSource) candidateProfilePaths(id string) []string {
	clean := filepath.Clean(id)
	return []string{
		filepath.Join(c.BundleRoot, "profiles", clean+".yaml"),
		filepath.Join(c.BundleRoot, "profiles", clean+".yml"),
		filepath.Join(c.BundleRoot, "profiles", clean+".md"),
		filepath.Join(c.BundleRoot, "parts", clean+".yaml"),
		filepath.Join(c.BundleRoot, "parts", clean+".md"),
	}
}

func (c *CairnBundleSource) GetProfile(_ context.Context, id string) (*LaunchProfile, error) {
	for _, p := range c.candidateProfilePaths(id) {
		b, err := os.ReadFile(p) //nolint:gosec // G304: bundle-sourced path
		if err != nil {
			continue
		}
		profile, err := parseProfileBytes(b, id)
		if err != nil {
			return nil, fmt.Errorf("parsing cairn profile from %s: %w", p, err)
		}
		return profile, nil
	}
	return nil, fmt.Errorf("%w: %q (in cairn bundle %s)", ErrProfileNotFound, id, c.BundleRoot)
}

func (c *CairnBundleSource) GetContext(_ context.Context, id string) (*LaunchContext, error) {
	// Cairn bundles do not define project scope/context (scope is launch input).
	return nil, fmt.Errorf("%w: %q (cairn bundles do not author contexts)", ErrContextNotFound, id)
}

// MultiSource chains multiple sources in priority order.
type MultiSource struct {
	Sources []Source
}

// NewMultiSource creates a MultiSource composed of the given sources.
func NewMultiSource(sources ...Source) *MultiSource {
	return &MultiSource{Sources: sources}
}

func (m *MultiSource) GetProfile(ctx context.Context, id string) (*LaunchProfile, error) {
	for _, src := range m.Sources {
		p, err := src.GetProfile(ctx, id)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrProfileNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrProfileNotFound, id)
}

func (m *MultiSource) GetContext(ctx context.Context, id string) (*LaunchContext, error) {
	for _, src := range m.Sources {
		c, err := src.GetContext(ctx, id)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, ErrContextNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrContextNotFound, id)
}

// parseProfileBytes decodes YAML or Markdown frontmatter into a LaunchProfile.
func parseProfileBytes(data []byte, defaultID string) (*LaunchProfile, error) {
	trimmed := bytes.TrimSpace(data)
	var frontmatter []byte
	var body string

	if bytes.HasPrefix(trimmed, []byte("---")) {
		// Markdown or YAML with frontmatter fence
		parts := bytes.SplitN(trimmed[3:], []byte("---"), 2)
		if len(parts) == 2 {
			frontmatter = bytes.TrimSpace(parts[0])
			body = string(bytes.TrimSpace(parts[1]))
		} else {
			frontmatter = trimmed
		}
	} else {
		frontmatter = trimmed
	}

	var p LaunchProfile
	if err := yaml.Unmarshal(frontmatter, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		p.ID = defaultID
	}
	if body != "" && p.Body == "" {
		p.Body = body
	}
	return &p, nil
}
