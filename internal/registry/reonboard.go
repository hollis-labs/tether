package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"gopkg.in/yaml.v3"
)

// ReonboardReport summarizes the results of re-onboarding legacy project rows (CW-20260914-0044).
type ReonboardReport struct {
	TotalProcessed int      `json:"total_processed"`
	Updated        int      `json:"updated"`
	Created        int      `json:"created"`
	Errors         []string `json:"errors,omitempty"`
}

// ReonboardProjects re-onboards project rows explicitly under the new contract (CW-20260914-0044).
// It migrates rows away from legacy passive bootstrap state ("system:bootstrap", "system:merge",
// repo_root in kind_meta) into the modern contract:
//   - Flat, open authored props bag (repo_root, tracking_root, tesseract_namespace)
//   - Substrate external ID attribution (substrate="tether")
//   - LastUpdatedBy stamped with "operator:re-onboard"
//   - Provenance tracking in field_metadata asserting FieldClassAuthored for props
func ReonboardProjects(ctx context.Context, svc *Service, catalogRoot string) (ReonboardReport, error) {
	if svc == nil {
		return ReonboardReport{}, fmt.Errorf("registry: reonboard projects: service required")
	}

	var report ReonboardReport

	// Pass 1: If catalogRoot is provided, process projects defined in catalog YAMLs.
	if catalogRoot != "" {
		projectsDir := filepath.Join(config.Expand(catalogRoot), "projects")
		entries, err := os.ReadDir(projectsDir)
		if err == nil {
			for _, ent := range entries {
				if ent.IsDir() || isBackupFile(ent.Name()) || !isYAMLFile(ent.Name()) {
					continue
				}
				report.TotalProcessed++
				path := filepath.Join(projectsDir, ent.Name())
				if err := reonboardCatalogFile(ctx, svc, path, &report); err != nil {
					report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", ent.Name(), err))
				}
			}
		}
	}

	// Pass 2: Sweep all existing project rows in the registry and upgrade any
	// remaining rows carrying legacy bootstrap authorship or kind_meta props.
	existingRows, err := svc.Search(ctx, KindProject, Filter{Status: StatusAny})
	if err != nil {
		return report, fmt.Errorf("registry: search existing projects: %w", err)
	}

	for _, row := range existingRows {
		needsUpgrade := row.LastUpdatedBy == "system:bootstrap" ||
			row.LastUpdatedBy == "system:merge" ||
			len(row.Props) == 0 ||
			len(row.KindMeta) > 0

		if !needsUpgrade {
			continue
		}

		if err := upgradeProjectRow(ctx, svc, row); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("upgrade %s: %v", row.URN, err))
		} else {
			report.Updated++
		}
	}

	return report, nil
}

// ReonboardProjects wraps ReonboardProjects on the Service.
func (s *Service) ReonboardProjects(ctx context.Context, catalogRoot string) (ReonboardReport, error) {
	return ReonboardProjects(ctx, s, catalogRoot)
}

func reonboardCatalogFile(ctx context.Context, svc *Service, path string, report *ReonboardReport) error {
	var p config.Project
	b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path; same trust boundary as internal/config/loader.go
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}

	slug := strings.TrimSpace(p.ID)
	if slug == "" {
		slug = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = slug
	}

	props := make(map[string]string)
	if p.RepoRoot != "" {
		props["repo_root"] = p.RepoRoot
	}
	if p.TrackingRoot != "" {
		props["tracking_root"] = p.TrackingRoot
	}
	props["tesseract_namespace"] = "user/chrispian/knowledge/" + slug

	// Check if already registered by external_id (tether: slug)
	existing, err := svc.LookupBy(ctx, KindProject, "tether", slug)
	if err == nil {
		// Existing row found: update it
		return updateRowToContract(ctx, svc, existing, name, props, slug)
	}

	// Try lookup by callback target
	abs, _ := filepath.Abs(path)
	target := "file://" + abs
	existing, err = svc.storage.FindByCallbackTarget(ctx, target)
	if err == nil {
		return updateRowToContract(ctx, svc, existing, name, props, slug)
	}

	// Not found: register fresh under new contract
	extIDs := []ExternalID{{
		Substrate:  "tether",
		ExternalID: slug,
		AttachedAt: time.Now().UTC(),
	}}
	_, err = svc.Register(ctx, KindProject, Profile{
		Owner:         "tether",
		DisplayName:   name,
		Props:         props,
		ExternalIDs:   extIDs,
		Callback:      &Callback{Scheme: "file", Target: target},
		LastUpdatedBy: "operator:re-onboard",
	})
	if err != nil {
		return fmt.Errorf("register fresh: %w", err)
	}
	report.Created++
	return nil
}

func updateRowToContract(ctx context.Context, svc *Service, row Profile, name string, props map[string]string, slug string) error {
	mergedProps := make(map[string]string)
	for k, v := range row.Props {
		mergedProps[k] = v
	}
	for k, v := range props {
		mergedProps[k] = v
	}

	propsJSON, err := json.Marshal(mergedProps)
	if err != nil {
		return err
	}

	fields := map[string]any{
		"props_json":      string(propsJSON),
		"last_updated_by": "operator:re-onboard",
	}
	if name != "" && row.DisplayName != name {
		fields["display_name"] = name
	}
	if row.Owner == "" {
		fields["owner"] = "tether"
	}

	// Synthesize updated field metadata with authored classification for props
	row.Props = mergedProps
	row.LastUpdatedBy = "operator:re-onboard"
	meta := SynthesizeFieldMetadata(row)
	metaJSON, _ := json.Marshal(meta)
	fields["field_metadata_json"] = string(metaJSON)

	if err := svc.storage.UpdateProfileFields(ctx, row.URN, fields); err != nil {
		return err
	}

	if slug != "" {
		_ = svc.AttachExternalID(ctx, row.URN, "tether", slug)
	}
	return nil
}

func upgradeProjectRow(ctx context.Context, svc *Service, row Profile) error {
	props := make(map[string]string)
	for k, v := range row.Props {
		props[k] = v
	}

	// Extract repo_root and tracking_root from legacy kind_meta if present
	if len(row.KindMeta) > 0 {
		var meta map[string]any
		if err := json.Unmarshal(row.KindMeta, &meta); err == nil {
			if rr, ok := meta["repo_root"].(string); ok && rr != "" && props["repo_root"] == "" {
				props["repo_root"] = rr
			}
			if tr, ok := meta["tracking_root"].(string); ok && tr != "" && props["tracking_root"] == "" {
				props["tracking_root"] = tr
			}
		}
	}

	// Ensure tesseract_namespace is populated
	if props["tesseract_namespace"] == "" {
		slug := ""
		if ext, ok := row.ExternalIDFor("tether"); ok {
			slug = ext.ExternalID
		} else if row.Project != "" {
			slug = row.Project
		} else {
			slug = strings.ToLower(strings.ReplaceAll(row.DisplayName, " ", "-"))
		}
		props["tesseract_namespace"] = "user/chrispian/knowledge/" + slug
	}

	propsJSON, err := json.Marshal(props)
	if err != nil {
		return err
	}

	fields := map[string]any{
		"props_json":      string(propsJSON),
		"last_updated_by": "operator:re-onboard",
	}
	if row.Owner == "" {
		fields["owner"] = "tether"
	}

	row.Props = props
	row.LastUpdatedBy = "operator:re-onboard"
	meta := SynthesizeFieldMetadata(row)
	metaJSON, _ := json.Marshal(meta)
	fields["field_metadata_json"] = string(metaJSON)

	return svc.storage.UpdateProfileFields(ctx, row.URN, fields)
}
