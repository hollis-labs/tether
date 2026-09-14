package registry

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/config"
	"gopkg.in/yaml.v3"
)

// BackfillTetherExternalIDs attaches substrate='tether' identifiers to rows
// imported from the local catalog. It is safe to re-run; already-attached rows
// are skipped.
func BackfillTetherExternalIDs(ctx context.Context, svc *Service, catalogRoot string) (int, error) {
	if svc == nil {
		return 0, fmt.Errorf("registry: backfill tether external ids: service required")
	}
	if catalogRoot == "" {
		return 0, fmt.Errorf("registry: backfill tether external ids: catalog root required")
	}

	catalogRoot = filepath.Clean(config.Expand(catalogRoot))
	var attached int
	for _, kind := range []Kind{KindAgent, KindProject} {
		rows, err := svc.Search(ctx, kind, Filter{Status: StatusAny})
		if err != nil {
			return attached, err
		}
		for _, row := range rows {
			if row.Callback == nil || row.Callback.Scheme != "file" {
				continue
			}
			path, ok := filePathFromTarget(row.Callback.Target)
			if !ok {
				continue
			}
			path = filepath.Clean(path)
			var expectedDir string
			if kind == KindAgent {
				expectedDir = filepath.Join(catalogRoot, "agents") + string(filepath.Separator)
			} else {
				expectedDir = filepath.Join(catalogRoot, "projects") + string(filepath.Separator)
			}
			if !strings.HasPrefix(path, expectedDir) {
				continue
			}
			if _, ok := row.ExternalIDFor("tether"); ok {
				continue
			}
			externalID, err := tetherExternalIDFromFile(kind, path)
			if err != nil {
				return attached, err
			}
			if err := svc.AttachExternalID(ctx, row.URN, "tether", externalID); err != nil {
				return attached, err
			}
			attached++
		}
	}
	return attached, nil
}

func (s *Service) BackfillTetherExternalIDs(ctx context.Context, catalogRoot string) (int, error) {
	return BackfillTetherExternalIDs(ctx, s, catalogRoot)
}

func tetherExternalIDFromFile(kind Kind, path string) (string, error) {
	switch kind {
	case KindAgent:
		var a config.Agent
		if err := loadYAMLFile(path, &a); err != nil {
			return "", fmt.Errorf("registry: backfill tether external ids: parse agent %s: %w", path, err)
		}
		if a.ID == "" {
			return "", fmt.Errorf("registry: backfill tether external ids: agent %s missing id", path)
		}
		return a.ID, nil
	case KindProject:
		var p config.Project
		if err := loadYAMLFile(path, &p); err != nil {
			return "", fmt.Errorf("registry: backfill tether external ids: parse project %s: %w", path, err)
		}
		if p.ID == "" {
			return "", fmt.Errorf("registry: backfill tether external ids: project %s missing id", path)
		}
		return p.ID, nil
	default:
		return "", fmt.Errorf("registry: backfill tether external ids: unsupported kind %q", kind)
	}
}

func filePathFromTarget(target string) (string, bool) {
	if strings.HasPrefix(target, "file://") {
		u, err := url.Parse(target)
		if err == nil && u.Path != "" {
			return u.Path, true
		}
		return strings.TrimPrefix(target, "file://"), true
	}
	return "", false
}

func loadYAMLFile(path string, out any) error {
	b, err := os.ReadFile(path) //nolint:gosec // bootstrap reads operator-owned catalog files
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

// BackfillOwnership inspects rows with empty owner and infers owner
// from callback target, external IDs, and kind_meta.
// Safe to re-run; rows with non-empty owner are skipped.
func BackfillOwnership(ctx context.Context, svc *Service) (int, error) {
	if svc == nil {
		return 0, fmt.Errorf("registry: backfill ownership: service required")
	}
	var backfilled int
	for _, kind := range []Kind{KindAgent, KindProject} {
		rows, err := svc.Search(ctx, kind, Filter{Status: StatusAny})
		if err != nil {
			return backfilled, err
		}
		for _, row := range rows {
			if row.Owner != "" {
				continue
			}
			inferred := inferOwner(row)
			if inferred == "" {
				continue
			}
			if err := svc.storage.UpdateProfileFields(ctx, row.URN, map[string]any{"owner": inferred}); err != nil {
				return backfilled, fmt.Errorf("registry: backfill ownership for %s: %w", row.URN, err)
			}
			backfilled++
		}
	}
	return backfilled, nil
}

func (s *Service) BackfillOwnership(ctx context.Context) (int, error) {
	return BackfillOwnership(ctx, s)
}

func inferOwner(row Profile) string {
	// 1. Callback target signal (strongest provenance signal)
	if row.Callback != nil && row.Callback.Target != "" {
		target := strings.ToLower(row.Callback.Target)
		if strings.Contains(target, ".cerberus/") || strings.Contains(target, "/cerberus/") {
			return "cerberus"
		}
		if strings.Contains(target, ".tether/") || strings.Contains(target, "/tether/") {
			return "tether"
		}
	}
	// 2. Substrate external IDs
	hasCerberus := false
	hasTether := false
	for _, ext := range row.ExternalIDs {
		switch ext.Substrate {
		case "cerberus":
			hasCerberus = true
		case "tether":
			hasTether = true
		}
	}
	if hasCerberus && !hasTether {
		return "cerberus"
	}
	if hasTether && !hasCerberus {
		return "tether"
	}
	// 3. KindMeta
	if len(row.KindMeta) > 0 {
		metaStr := strings.ToLower(string(row.KindMeta))
		if strings.Contains(metaStr, `"cerberus"`) {
			return "cerberus"
		}
	}
	// 4. Default for Tether-hosted store entries if they have a file callback
	if hasTether || (row.Callback != nil && row.Callback.Scheme == "file") {
		return "tether"
	}
	return ""
}
