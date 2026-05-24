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
