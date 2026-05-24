package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const cerberusProjectKind = "cerberus-project/v1"

type cerberusIndex struct {
	Entries []cerberusIndexEntry `yaml:"entries"`
}

type cerberusIndexEntry struct {
	Owner      string `yaml:"owner"`
	Namespace  string `yaml:"namespace"`
	Path       string `yaml:"path"`
	Kind       string `yaml:"kind"`
	Registered string `yaml:"registered_at"`
}

type cerberusProjectFile struct {
	RegistryURN string `yaml:"registry_urn"`
	Project     struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	} `yaml:"project"`
	Resources []any `yaml:"resources"`
}

// BootstrapFromCerberus imports Cerberus registry index entries into the local
// registry, attaching substrate='cerberus' to overlapping rows instead of
// registering duplicates.
func BootstrapFromCerberus(ctx context.Context, svc *Service, cerberusHome string, force bool, writeBack bool) (BootstrapReport, error) {
	if svc == nil {
		return BootstrapReport{}, errors.New("registry: cerberus bootstrap: service required")
	}
	if cerberusHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return BootstrapReport{}, fmt.Errorf("registry: cerberus bootstrap: resolve home: %w", err)
		}
		cerberusHome = filepath.Join(home, ".cerberus")
	}

	indexPath := filepath.Join(cerberusHome, "registry.yaml")
	var idx cerberusIndex
	if err := loadYAMLFile(indexPath, &idx); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BootstrapReport{}, nil
		}
		return BootstrapReport{}, fmt.Errorf("registry: cerberus bootstrap: load index: %w", err)
	}

	var report BootstrapReport
	for _, entry := range idx.Entries {
		if entry.Kind != cerberusProjectKind {
			continue
		}
		var project cerberusProjectFile
		if err := loadYAMLFile(entry.Path, &project); err != nil {
			report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
			continue
		}
		if entry.Owner == "" || project.Project.Name == "" {
			report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: "missing owner or project.name"})
			continue
		}

		existing, err := svc.LookupBy(ctx, KindProject, entry.Owner, "cerberus")
		switch {
		case err == nil:
			if force {
				if err := refreshCerberusProfile(ctx, svc, existing.URN, project.Project.Name, entry, len(project.Resources)); err != nil {
					report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
					continue
				}
			}
			if writeBack {
				if err := writeURNBackSafe(entry.Path, existing.URN); err != nil {
					report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
					continue
				}
			}
			report.Skipped++
			continue
		case !errors.Is(err, ErrNotFound):
			return report, err
		}

		target, err := svc.LookupBy(ctx, KindProject, entry.Owner, "")
		switch {
		case err == nil:
			if err := svc.AttachExternalID(ctx, target.URN, "cerberus", entry.Owner); err != nil {
				report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
				continue
			}
			if err := refreshCerberusProfile(ctx, svc, target.URN, project.Project.Name, entry, len(project.Resources)); err != nil {
				report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
				continue
			}
			if writeBack {
				if err := writeURNBackSafe(entry.Path, target.URN); err != nil {
					report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
					continue
				}
			}
			report.Attached++
			continue
		case !errors.Is(err, ErrNotFound):
			return report, err
		}

		meta, err := cerberusKindMeta(entry, len(project.Resources))
		if err != nil {
			report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
			continue
		}
		created, err := svc.Register(ctx, KindProject, Profile{
			DisplayName: project.Project.Name,
			Callback: &Callback{
				Scheme: "file",
				Target: "file://" + entry.Path,
			},
			KindMeta:      meta,
			LastUpdatedBy: bootstrapLastUpdatedBy,
		})
		if err != nil {
			report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
			continue
		}
		if err := svc.AttachExternalID(ctx, created.URN, "cerberus", entry.Owner); err != nil {
			report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
			continue
		}
		if writeBack {
			if err := writeURNBackSafe(entry.Path, created.URN); err != nil {
				report.Errors = append(report.Errors, BootstrapError{Path: entry.Path, Reason: err.Error()})
				continue
			}
		}
		report.Imported++
	}
	return report, nil
}

func (s *Service) BootstrapFromCerberus(ctx context.Context, cerberusHome string, force bool, writeBack bool) (BootstrapReport, error) {
	return BootstrapFromCerberus(ctx, s, cerberusHome, force, writeBack)
}

func refreshCerberusProfile(ctx context.Context, svc *Service, urn, displayName string, entry cerberusIndexEntry, resourcesCount int) error {
	meta, err := cerberusKindMeta(entry, resourcesCount)
	if err != nil {
		return err
	}
	cb, _ := json.Marshal(&Callback{Scheme: "file", Target: "file://" + entry.Path})
	fields := map[string]any{
		"display_name":    displayName,
		"callback_json":   string(cb),
		"kind_meta_json":  string(meta),
		"last_updated_by": bootstrapLastUpdatedBy,
	}
	return svc.storage.UpdateProfileFields(ctx, urn, fields)
}

func cerberusKindMeta(entry cerberusIndexEntry, resourcesCount int) (json.RawMessage, error) {
	meta := map[string]any{
		"cerberus": map[string]any{
			"namespace":              entry.Namespace,
			"source_path":            entry.Path,
			"resources_count":        resourcesCount,
			"cerberus_registered_at": entry.Registered,
		},
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal kind_meta: %w", err)
	}
	return json.RawMessage(b), nil
}

func writeURNBackSafe(path, urn string) error {
	err := WriteURNBack(path, urn)
	if errors.Is(err, errURNMismatch) {
		return nil
	}
	return err
}
