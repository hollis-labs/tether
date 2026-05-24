package registry

// bootstrap.go — catalog importer (T-v060-01-08). Walks
// ~/.tether/catalog/{agents,projects}/*.yaml on first daemon start (and on
// operator demand via `mux registry bootstrap --force`) and lands a thin
// public-identity row per file. Source YAMLs remain the truth; the
// registry holds only the identity projection + a file:// callback that
// Sync re-reads on demand (D18).
//
// Identity-only discipline (D12 + forward-looking discipline for v060-02).
// The importer extracts identity fields exclusively. It MUST NOT copy
// `resources[]`, `config`, `env`, secrets, or any operational payload into
// the registry row. Tether's own catalog YAMLs don't carry secrets today,
// but v060-02's cerberus importer inherits this contract — cerberus catalog
// files commonly contain plaintext OAuth tokens, and the registry never
// echoes those bytes. D18 dropped the cached_payload_json column from the
// schema to enforce this at the storage level; bootstrap respects the same
// fence in code by ignoring everything outside the documented projection.
//
// Idempotency. Each parsed file is mapped to a canonical Callback.Target
// (`file://<abs-path>`) and looked up via Storage.FindByCallbackTarget. A
// match means "this source file is already imported"; the bootstrap skips
// it on force=false. On force=true the row is patched with the refreshed
// thin profile via Service.UpdateSelf and cached_at is bumped via the
// storage's BumpCachedAt — equivalent to a successful Sync, but without
// invoking the resolver: bootstrap source files are in config.Agent /
// config.Project shape, not the Profile shape Sync expects, so a direct
// callback round-trip would fail the Profile-decoder. The thin-profile
// projection has already been applied at the UpdateSelf step; bumping
// cached_at by hand stamps "this row's data is fresh as of now" without
// requiring a second translation pass on the same bytes we just parsed.
//
// Dedup approach. We match on the canonical callback.target string rather
// than threading a separate `kind_meta.source_path` field — the callback
// already carries the abs path, and duplicating it in kind_meta would
// risk drift between the two. The cross-substrate dedup primitive
// (LookupBy with external_id + substrate attribution) is a v060-02
// concern; v1 only needs "same Tether file path" idempotency.
//
// Skip-on-error. One malformed YAML does not abort the bootstrap. Each
// per-file failure (parse, lookup, register, update) is captured in
// BootstrapReport.Errors and the next file proceeds. Only fatal errors
// from the surrounding scan (e.g. catalog root unreadable for an unknown
// reason) propagate out as the function's error return.
//
// Backup files. Files whose basename matches the `*.bak-*` convention
// (used by the catalog edit workflow to retain pre-edit copies) are
// skipped silently — no entry in Errors, not counted in Imported /
// Skipped / Refreshed.
//
// Missing subdirs. A catalog root with no agents/ or no projects/
// subdirectory is fine: zero files, no errors. The bootstrap is a
// catalog-shape-tolerant best-effort import, not a structural validator.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/config"
)

// BootstrapReport summarizes a bootstrap pass: how many rows landed, how
// many were skipped as already-imported, how many were refreshed on a
// force=true run, and per-file errors. Returned by both the package-level
// BootstrapFromCatalog and Service.BootstrapFromCatalog wrappers; consumed
// by the daemon log on startup and by the `mux registry bootstrap` CLI.
type BootstrapReport struct {
	// Imported is the count of fresh rows registered this pass.
	Imported int
	// Attached is the count of external-id attachments added to existing rows.
	Attached int
	// Skipped is the count of existing rows left unchanged (force=false).
	Skipped int
	// Refreshed is the count of existing rows patched + sync-stamped
	// (force=true). Always 0 on a force=false run.
	Refreshed int
	// Errors collects per-file failures. Empty on a clean run.
	Errors []BootstrapError
}

// BootstrapError records a single per-file failure. Path is the absolute
// path the importer tried to process; Reason is a short, human-readable
// description (e.g. "parse yaml: …", "register agent: …").
type BootstrapError struct {
	Path   string
	Reason string
}

// bootstrapLastUpdatedBy is the placeholder identity stamped on rows the
// importer touches. Matches the pattern Service.Register uses for its own
// implicit identity ("system:register") and Service.Sync uses for
// callback-driven refreshes ("system:sync"). v060-02's token-based
// identity supersedes all three.
const bootstrapLastUpdatedBy = "system:bootstrap"

// BootstrapFromCatalog imports every <catalogRoot>/agents/*.yaml and
// <catalogRoot>/projects/*.yaml into svc. Rows are matched by canonical
// callback.target ("file://<abs-path>"); existing matches are skipped
// when force=false and refreshed when force=true. The function never
// returns a non-nil error for per-file problems — those land in the
// report's Errors slice and the bootstrap continues to the next file.
//
// Behavior summary:
//
//	force=false (daemon-startup default)
//	  - new files → Register, Imported++
//	  - existing files → skip, Skipped++
//	  - bad files → report.Errors, scan continues
//
//	force=true (operator-driven catalog-drift refresh)
//	  - new files → Register, Imported++
//	  - existing files → UpdateSelf + BumpCachedAt, Refreshed++
//	  - bad files → report.Errors, scan continues
//
// Returns a fatal error only when something outside the per-file scan
// fails irrecoverably — currently never, but the signature stays open
// for future composition (audit hooks, batch-tx wrappers).
func BootstrapFromCatalog(ctx context.Context, svc *Service, catalogRoot string, force bool) (BootstrapReport, error) {
	if svc == nil {
		return BootstrapReport{}, errors.New("registry: bootstrap: service required")
	}
	if catalogRoot == "" {
		return BootstrapReport{}, errors.New("registry: bootstrap: catalog root required")
	}

	report := BootstrapReport{}

	// agents → KindAgent translation.
	agentDir := filepath.Join(catalogRoot, "agents")
	importDir(ctx, svc, &report, agentDir, KindAgent, force, agentProfileFromFile)

	// projects → KindProject translation.
	projectDir := filepath.Join(catalogRoot, "projects")
	importDir(ctx, svc, &report, projectDir, KindProject, force, projectProfileFromFile)

	return report, nil
}

// BootstrapFromCatalog is the convenience method form. Identical to the
// package-level function; the daemon startup path calls this so the
// composition root only needs the Service handle.
func (s *Service) BootstrapFromCatalog(ctx context.Context, catalogRoot string, force bool) (BootstrapReport, error) {
	return BootstrapFromCatalog(ctx, s, catalogRoot, force)
}

// fileToProfile is the per-kind translator signature: given an absolute
// path to a YAML file, parse it and return a thin Profile + Callback
// suitable for Register / UpdateSelf. Errors propagate verbatim.
type fileToProfile func(absPath string) (Profile, error)

// importDir walks dir, applies translate to each non-backup YAML, and
// routes the result to Register (new) or UpdateSelf+BumpCachedAt
// (force=true) or skip (force=false). Missing dir → no-op. Per-file
// errors land in the report's Errors slice.
func importDir(
	ctx context.Context,
	svc *Service,
	report *BootstrapReport,
	dir string,
	kind Kind,
	force bool,
	translate fileToProfile,
) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing subdir is normal (the catalog might only have agents/
		// or only projects/). Anything else is also non-fatal — the
		// rest of the import should still proceed; record the directory
		// itself as the failing path and move on.
		if !errors.Is(err, os.ErrNotExist) {
			report.Errors = append(report.Errors, BootstrapError{
				Path:   dir,
				Reason: fmt.Sprintf("read dir: %v", err),
			})
		}
		return
	}

	// Stable order so the daemon log is deterministic.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isYAMLFile(name) {
			continue
		}
		if isBackupFile(name) {
			continue
		}
		path := filepath.Join(dir, name)
		importFile(ctx, svc, report, path, kind, force, translate)
	}
}

// importFile applies the per-file translation, looks up by callback
// target, and dispatches to Register / UpdateSelf+Sync / skip. Encapsulated
// so the per-kind loops stay flat in importDir.
func importFile(
	ctx context.Context,
	svc *Service,
	report *BootstrapReport,
	path string,
	kind Kind,
	force bool,
	translate fileToProfile,
) {
	profile, err := translate(path)
	if err != nil {
		report.Errors = append(report.Errors, BootstrapError{
			Path:   path,
			Reason: err.Error(),
		})
		return
	}
	if profile.Callback == nil {
		report.Errors = append(report.Errors, BootstrapError{
			Path:   path,
			Reason: "translate: callback unset (importer invariant)",
		})
		return
	}

	existing, lookupErr := svc.storage.FindByCallbackTarget(ctx, profile.Callback.Target)
	switch {
	case lookupErr == nil:
		// Existing row.
		if !force {
			report.Skipped++
			return
		}
		// force=true: patch the row with the refreshed thin profile, then
		// bump cached_at directly. UpdateSelf is full-replace on the array
		// fields per D5; the cached_at bump stamps "this row's projection
		// is fresh as of now". We do NOT call Service.Sync here — the
		// resolver pipeline expects a Profile-shaped payload on the other
		// side of the callback, but bootstrap's source files are in
		// config.Agent / config.Project shape and would fail Profile
		// decode. The semantically right thing is "update + mark fresh",
		// which is what we do.
		patch := buildBootstrapPatch(profile)
		if _, err := svc.UpdateSelf(ctx, existing.URN, patch); err != nil {
			report.Errors = append(report.Errors, BootstrapError{
				Path:   path,
				Reason: fmt.Sprintf("update_self %s: %v", existing.URN, err),
			})
			return
		}
		if err := svc.storage.BumpCachedAt(ctx, existing.URN, time.Now().UTC()); err != nil {
			report.Errors = append(report.Errors, BootstrapError{
				Path:   path,
				Reason: fmt.Sprintf("bump cached_at %s: %v", existing.URN, err),
			})
			return
		}
		report.Refreshed++
		return

	case errors.Is(lookupErr, ErrNotFound):
		// New row → Register.
		if _, err := svc.Register(ctx, kind, profile); err != nil {
			report.Errors = append(report.Errors, BootstrapError{
				Path:   path,
				Reason: fmt.Sprintf("register %s: %v", kind, err),
			})
			return
		}
		report.Imported++
		return

	default:
		report.Errors = append(report.Errors, BootstrapError{
			Path:   path,
			Reason: fmt.Sprintf("lookup by callback: %v", lookupErr),
		})
		return
	}
}

// agentProfileFromFile parses path as a config.Agent YAML and translates
// it to the v060-01 thin profile shape. Identity-only: Name, Roles,
// Skills, and the abs-path callback are the only fields that cross the
// substrate boundary into the registry.
func agentProfileFromFile(absPath string) (Profile, error) {
	var a config.Agent
	if err := readYAMLFile(absPath, &a); err != nil {
		return Profile{}, fmt.Errorf("parse agent yaml: %w", err)
	}

	// DisplayName: prefer human Name, fall back to the catalog ID, fall
	// back to the file basename without extension. Service.Register
	// rejects an empty display_name, so the final fallback is a sanity
	// net — a YAML with neither Name nor ID still produces a registrable
	// row keyed on the source file.
	display := strings.TrimSpace(a.Name)
	if display == "" {
		display = strings.TrimSpace(a.ID)
	}
	if display == "" {
		display = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
	}

	// Primary role + secondary roles (kind_meta).
	role := ""
	var secondaryRoles []string
	if len(a.Roles) > 0 {
		role = a.Roles[0]
		if len(a.Roles) > 1 {
			secondaryRoles = append(secondaryRoles, a.Roles[1:]...)
		}
	}

	kindMeta, err := json.Marshal(map[string]any{
		"roles": secondaryRoles,
	})
	if err != nil {
		return Profile{}, fmt.Errorf("marshal kind_meta: %w", err)
	}

	abs, err := filepath.Abs(absPath)
	if err != nil {
		return Profile{}, fmt.Errorf("abs path: %w", err)
	}

	return Profile{
		DisplayName:   display,
		Role:          role,
		Capabilities:  append([]string(nil), a.Skills...), // copy so the slice isn't aliased to the YAML decode
		KindMeta:      kindMeta,
		Callback:      &Callback{Scheme: "file", Target: "file://" + abs},
		LastUpdatedBy: bootstrapLastUpdatedBy,
	}, nil
}

// projectProfileFromFile parses path as a config.Project YAML and
// translates it to the v060-01 thin profile shape. Identity-only:
// DisplayName + the path-shaped kind_meta + the abs-path callback. No
// role on projects.
func projectProfileFromFile(absPath string) (Profile, error) {
	var p config.Project
	if err := readYAMLFile(absPath, &p); err != nil {
		return Profile{}, fmt.Errorf("parse project yaml: %w", err)
	}

	display := strings.TrimSpace(p.Name)
	if display == "" {
		display = strings.TrimSpace(p.ID)
	}
	if display == "" {
		display = strings.TrimSuffix(filepath.Base(absPath), filepath.Ext(absPath))
	}

	kindMeta, err := json.Marshal(map[string]any{
		"repo_root":            p.RepoRoot,
		"tracking_root":        p.TrackingRoot,
		"knowledge_base_paths": p.KnowledgeBase,
	})
	if err != nil {
		return Profile{}, fmt.Errorf("marshal kind_meta: %w", err)
	}

	abs, err := filepath.Abs(absPath)
	if err != nil {
		return Profile{}, fmt.Errorf("abs path: %w", err)
	}

	return Profile{
		DisplayName:   display,
		KindMeta:      kindMeta,
		Callback:      &Callback{Scheme: "file", Target: "file://" + abs},
		LastUpdatedBy: bootstrapLastUpdatedBy,
	}, nil
}

// buildBootstrapPatch translates a parsed Profile into a full-replace
// UpdatePatch suitable for force=true refresh. Mirrors buildSyncPatch in
// service.go but stamps LastUpdatedBy with the bootstrap identity (so the
// row's audit trail distinguishes "operator re-ran bootstrap" from
// "callback Sync fired").
func buildBootstrapPatch(p Profile) UpdatePatch {
	patch := UpdatePatch{LastUpdatedBy: bootstrapLastUpdatedBy}
	if p.DisplayName != "" {
		v := p.DisplayName
		patch.DisplayName = &v
	}
	if p.Role != "" {
		v := p.Role
		patch.Role = &v
	}
	// Empty role on projects: we explicitly want to leave the column alone
	// rather than clear it (a nil pointer = "no change"; a pointer to ""
	// would clear). The Role omission above does that automatically.
	if len(p.KindMeta) > 0 {
		patch.KindMeta = p.KindMeta
	}
	patch.Capabilities = &ArrayPatch[string]{
		Mode:  ArrayModeReplace,
		Value: p.Capabilities,
	}
	// Skills + Links aren't populated by bootstrap (D13 + future link
	// curation), but the full-replace mode is the correct projection
	// shape — len==0 hits UpdateSelf's no-op path so we don't accidentally
	// wipe operator-curated additions. See Sync's "Empty-array nuance"
	// godoc for the same observation.
	return patch
}

// readYAMLFile reads + unmarshals a YAML file into out. Mirrors
// config.loadYAML but lives in this package so bootstrap.go doesn't
// pull internal/config's other concerns (layered loading, validation).
func readYAMLFile(path string, out any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path; same trust boundary as internal/config/loader.go
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// isYAMLFile reports whether name has a YAML extension (.yaml or .yml).
// Other extensions (and extensionless files) are skipped by the importer.
func isYAMLFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// isBackupFile reports whether name matches the *.bak-* convention used
// by the catalog edit workflow (e.g. agents/foo.yaml.bak-20260520T1200).
// strings.Contains is used rather than filepath.Match because the suffix
// shape varies (timestamp, hash, free-form annotation) and we just need
// to detect the ".bak-" marker anywhere in the name.
func isBackupFile(name string) bool {
	return strings.Contains(name, ".bak-")
}
