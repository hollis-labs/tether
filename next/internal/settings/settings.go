// Package settings decodes Tether's own config block (the `tether:` key of the manifest, overlaid by
// the file named by SUBSTRATE_CONFIG) and refuses a bad value at start with a message that names the key.
package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Key is the block's name in app.Config: the manifest's app name.
const Key = "tether"

// Settings is the validated block. Slices add keys as they need them, each with the old tree's default
// and semantics (a test pins them); every key is documented in docs/architecture.md.
type Settings struct {
	// CatalogDir is the git-backed catalog root. Empty until the catalog slice wires it; when set it
	// must be absolute, because clients name the same directory by path (port plan section 3).
	CatalogDir string `json:"catalog_dir"`
}

// Defaults are the values a missing key takes.
func Defaults() Settings { return Settings{} }

// Parse decodes raw (app.Config[Key]) over the defaults. An unknown key, a value of the wrong type or a
// value out of range is an error naming the key.
func Parse(raw any) (Settings, error) {
	s := Defaults()
	if raw == nil {
		return s, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return s, fmt.Errorf("%s: must be a map, got %T", Key, raw)
	}
	for k, v := range m {
		b, err := json.Marshal(v)
		if err != nil {
			return s, fmt.Errorf("%s.%s: %w", Key, k, err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		switch k {
		case "catalog_dir":
			err = dec.Decode(&s.CatalogDir)
		default:
			return s, fmt.Errorf("%s.%s: unknown key", Key, k)
		}
		if err != nil {
			return s, fmt.Errorf("%s.%s: %w", Key, k, err)
		}
	}
	if s.CatalogDir != "" && !filepath.IsAbs(s.CatalogDir) {
		return s, fmt.Errorf("%s.catalog_dir: must be an absolute path, got %q", Key, s.CatalogDir)
	}
	return s, nil
}
