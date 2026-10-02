package mcptransport

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/hollis-labs/tether/internal/config"
)

// CatalogCache reparses only when metadata in an authority-bearing layer
// changes. An inaccessible or concurrently edited generation is unavailable,
// never permission to serve stale grants. Returned catalogs are immutable.
type CatalogCache struct {
	Root       string
	mu         sync.Mutex
	catalog    *config.Catalog
	generation string
}

func (c *CatalogCache) Load(ctx context.Context) (*config.Catalog, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	generation, err := c.fingerprint(ctx, c.catalog)
	if err != nil {
		return nil, err
	}
	if c.catalog != nil && generation == c.generation {
		return c.catalog, nil
	}
	cat, err := config.LoadLayered(c.Root)
	if err != nil {
		return nil, err
	}
	// New project registrations may add authority-bearing layer roots.
	before, err := c.fingerprint(ctx, cat)
	if err != nil {
		return nil, err
	}
	// Re-read when roots changed, so the parsed value covers the exact generation.
	cat, err = config.LoadLayered(c.Root)
	if err != nil {
		return nil, err
	}
	after, err := c.fingerprint(ctx, cat)
	if err != nil || before != after {
		return nil, fmt.Errorf("MCP catalog generation unavailable")
	}
	c.catalog = cat
	c.generation = after
	return cat, nil
}

func (c *CatalogCache) fingerprint(ctx context.Context, cat *config.Catalog) (string, error) {
	roots := []string{config.Expand(c.Root)}
	for _, layer := range config.LayeredCatalogLayers(cat) {
		roots = append(roots, filepath.Join(layer.Root, "agents"), filepath.Join(layer.Root, "skills"), filepath.Join(layer.Root, "boot-profiles"))
	}
	// Configured catalog subdirectories may live outside the catalog root.
	if cat != nil {
		for _, root := range []string{cat.Global.Catalog.Roots.Agents, cat.Global.Catalog.Roots.Projects, cat.Global.Catalog.Roots.Providers, cat.Global.Catalog.Roots.Launches} {
			if root != "" {
				if !filepath.IsAbs(root) {
					root = filepath.Join(config.Expand(c.Root), root)
				}
				roots = append(roots, root)
			}
		}
	}
	for _, dir := range []string{"projects", "agents", "providers", "launches", "mcp-servers", "boot-profiles", "sandbox-profiles"} {
		roots = append(roots, filepath.Join(config.Expand(c.Root), dir))
	}
	sort.Strings(roots)
	digest := sha256.New()
	for _, root := range roots {
		canonical, err := filepath.EvalSymlinks(root)
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintln(digest, root, "missing")
			continue
		}
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintln(digest, root, canonical)
		err = filepath.WalkDir(canonical, func(path string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if os.IsNotExist(err) {
				_, _ = fmt.Fprintln(digest, path, "missing")
				return nil
			}
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(digest, path, info.Mode(), info.Size(), info.ModTime().UnixNano())
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Stat(path)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintln(digest, target.Mode(), target.Size(), target.ModTime().UnixNano())
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}
