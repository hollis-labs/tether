package registry_test

// bootstrap_test.go — coverage for the catalog importer (T-v060-01-08).
//
// Matrix (per the sprint acceptance criteria):
//   - First-start happy path: a populated fixture catalog imports exactly
//     the right number of agent + project rows, each with a file://
//     callback and a derived thin profile.
//   - Idempotency: running BootstrapFromCatalog twice with force=false
//     yields 0 imported / N skipped on the second pass and the row
//     identities (URN, updated_at) don't drift.
//   - force=true refresh: editing a fixture YAML's `name:` between passes
//     and re-running with force=true updates display_name on the existing
//     row (URN preserved), and cached_at is bumped via the Sync hook.
//   - Skip-on-error: a malformed YAML is captured in report.Errors but
//     the other files import correctly — one bad apple does not abort
//     the bootstrap.
//   - Backup files: a *.bak-* sibling never produces a registry row
//     and does not appear in report.Errors.
//   - Missing catalog subdirs: a catalog with only agents/ (no
//     projects/) imports agents without error.
//   - Identity-only discipline: kind_meta carries the documented
//     identity bits but never operational shape (env, resources).
//
// Fixture strategy. Each test builds its own catalog dir under t.TempDir
// so cases don't bleed and run-order doesn't matter. The Service is
// always built with a real *Storage on in-memory SQLite (mirrors
// service_test.go's newService helper).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

// writeBootstrapFile writes content under dir/name, creating dir if
// missing. Lifted so each test stays focused on intent.
func writeBootstrapFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// newCatalogRoot builds a minimal {agents/, projects/} fixture and
// returns the root. Counts: 2 agents, 2 projects, 1 *.bak-* backup, 1
// malformed YAML. Returns the root + the canonical Profile-derived
// callback targets so tests can assert against them.
func newCatalogRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// Two valid agents.
	writeBootstrapFile(t, filepath.Join(root, "agents"), "alpha.yaml", `id: alpha
name: Alpha Agent
roles: [implementer, reviewer]
skills: [go, sqlite]
`)
	writeBootstrapFile(t, filepath.Join(root, "agents"), "beta.yaml", `id: beta
name: Beta Agent
roles: [architect]
skills: [planning]
`)

	// One backup that must be skipped silently.
	writeBootstrapFile(t, filepath.Join(root, "agents"), "alpha.yaml.bak-20260520",
		`id: alpha
name: STALE - should not import
`)

	// Two valid projects.
	writeBootstrapFile(t, filepath.Join(root, "projects"), "tether.yaml", `id: tether
name: Tether
repo_root: /repos/tether
tracking_root: /tracking/tether
knowledge_base:
  - /kb/tether/notes
  - /kb/tether/adrs
`)
	writeBootstrapFile(t, filepath.Join(root, "projects"), "agridd.yaml", `id: agridd
name: Agridd
repo_root: /repos/agridd
tracking_root: /tracking/agridd
`)

	// One malformed YAML — emits a parse error into report.Errors but
	// must not abort the rest of the bootstrap. Unbalanced flow-style
	// brackets reliably fail yaml.v3's parser; many other malformed
	// shapes are tolerated as bare strings.
	writeBootstrapFile(t, filepath.Join(root, "projects"), "broken.yaml", "name: [unbalanced\n")

	return root
}

func TestBootstrap_FirstStart_ImportsAllValidFiles(t *testing.T) {
	svc := newService(t)
	root := newCatalogRoot(t)

	report, err := registry.BootstrapFromCatalog(context.Background(), svc, root, false)
	if err != nil {
		t.Fatalf("BootstrapFromCatalog: %v", err)
	}

	// 2 agents + 2 projects valid; 1 broken project; 1 *.bak-* skipped silently.
	if report.Imported != 4 {
		t.Errorf("Imported = %d; want 4 (2 agents + 2 projects)", report.Imported)
	}
	if report.Skipped != 0 {
		t.Errorf("Skipped = %d; want 0 on first run", report.Skipped)
	}
	if report.Refreshed != 0 {
		t.Errorf("Refreshed = %d; want 0 on force=false", report.Refreshed)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("Errors = %d; want 1 (malformed projects/broken.yaml). Got: %+v", len(report.Errors), report.Errors)
	}
	if !strings.Contains(report.Errors[0].Path, "broken.yaml") {
		t.Errorf("Errors[0].Path = %q; want it to reference broken.yaml", report.Errors[0].Path)
	}

	// Verify rows reachable via Search.
	agents, err := svc.Search(context.Background(), registry.KindAgent, registry.Filter{})
	if err != nil {
		t.Fatalf("Search agents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("Search agents = %d; want 2", len(agents))
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.DisplayName)
	}
	if !contains(names, "Alpha Agent") || !contains(names, "Beta Agent") {
		t.Errorf("agent display_names = %v; want Alpha Agent + Beta Agent", names)
	}

	// Spot-check the alpha agent: primary role = implementer, capabilities = [go, sqlite],
	// secondary roles in kind_meta = [reviewer], callback = file://<abs alpha.yaml>.
	var alpha registry.Profile
	for _, a := range agents {
		if a.DisplayName == "Alpha Agent" {
			alpha = a
			break
		}
	}
	if alpha.Role != "implementer" {
		t.Errorf("alpha.Role = %q; want implementer", alpha.Role)
	}
	if !equalUnordered(alpha.Capabilities, []string{"go", "sqlite"}) {
		t.Errorf("alpha.Capabilities = %v; want [go sqlite]", alpha.Capabilities)
	}
	if alpha.Callback == nil || alpha.Callback.Scheme != "file" {
		t.Fatalf("alpha.Callback = %+v; want scheme=file", alpha.Callback)
	}
	expectedAbs, _ := filepath.Abs(filepath.Join(root, "agents", "alpha.yaml"))
	if alpha.Callback.Target != "file://"+expectedAbs {
		t.Errorf("alpha.Callback.Target = %q; want file://%s", alpha.Callback.Target, expectedAbs)
	}
	if alpha.LastUpdatedBy != "system:bootstrap" {
		t.Errorf("alpha.LastUpdatedBy = %q; want system:bootstrap", alpha.LastUpdatedBy)
	}

	// Kind-meta carries the documented identity bits — secondary roles for
	// agents, no operational shape (env, resources).
	var meta map[string]any
	if err := json.Unmarshal(alpha.KindMeta, &meta); err != nil {
		t.Fatalf("unmarshal alpha.KindMeta: %v", err)
	}
	if _, hasEnv := meta["env"]; hasEnv {
		t.Errorf("alpha.KindMeta carries env: %v (D12 violation)", meta)
	}
	if _, hasRes := meta["resources"]; hasRes {
		t.Errorf("alpha.KindMeta carries resources: %v (D12 violation)", meta)
	}
	if roles, _ := meta["roles"].([]any); len(roles) != 1 || roles[0] != "reviewer" {
		t.Errorf("alpha.KindMeta.roles = %v; want [reviewer]", meta["roles"])
	}

	// Backup file must NOT have produced a row.
	allAgents, _ := svc.Search(context.Background(), registry.KindAgent, registry.Filter{Status: registry.StatusAny})
	for _, a := range allAgents {
		if strings.Contains(a.DisplayName, "STALE") {
			t.Errorf("backup file produced a row: %+v", a)
		}
		if a.Callback != nil && strings.Contains(a.Callback.Target, ".bak-") {
			t.Errorf("backup callback target landed: %q", a.Callback.Target)
		}
	}

	// Project spot-check.
	projects, err := svc.Search(context.Background(), registry.KindProject, registry.Filter{})
	if err != nil {
		t.Fatalf("Search projects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("Search projects = %d; want 2", len(projects))
	}
	var tether registry.Profile
	for _, p := range projects {
		if p.DisplayName == "Tether" {
			tether = p
			break
		}
	}
	if tether.Role != "" {
		t.Errorf("project Role = %q; want empty (projects don't have a role)", tether.Role)
	}
	var pMeta map[string]any
	if err := json.Unmarshal(tether.KindMeta, &pMeta); err != nil {
		t.Fatalf("unmarshal tether.KindMeta: %v", err)
	}
	if pMeta["repo_root"] != "/repos/tether" {
		t.Errorf("tether.KindMeta.repo_root = %v; want /repos/tether", pMeta["repo_root"])
	}
	if pMeta["tracking_root"] != "/tracking/tether" {
		t.Errorf("tether.KindMeta.tracking_root = %v; want /tracking/tether", pMeta["tracking_root"])
	}
	kb, ok := pMeta["knowledge_base_paths"].([]any)
	if !ok || len(kb) != 2 {
		t.Errorf("tether.KindMeta.knowledge_base_paths = %v; want 2-element list", pMeta["knowledge_base_paths"])
	}
}

func TestBootstrapFromCerberus_DedupsAgainstTetherAndWritesURN(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	tetherRoot := t.TempDir()
	writeBootstrapFile(t, filepath.Join(tetherRoot, "projects"), "clockwork.yaml", `id: clockwork
name: Clockwork
repo_root: /repos/clockwork
tracking_root: /tracking/clockwork
`)

	if _, err := registry.BootstrapFromCatalog(ctx, svc, tetherRoot, false); err != nil {
		t.Fatalf("BootstrapFromCatalog: %v", err)
	}
	if _, err := registry.BackfillTetherExternalIDs(ctx, svc, tetherRoot); err != nil {
		t.Fatalf("BackfillTetherExternalIDs: %v", err)
	}

	cerberusHome := t.TempDir()
	projectPath := writeBootstrapFile(t, cerberusHome, "clockwork.cerberus.yaml", `kind: cerberus-project/v1
owner: clockwork
namespace: local
project:
  id: clockwork
  name: Clockwork
resources:
  - id: api
    type: service
    connector: noop
`)
	writeBootstrapFile(t, cerberusHome, "registry.yaml", `version: 1
entries:
  - owner: clockwork
    namespace: local
    path: `+projectPath+`
    kind: cerberus-project/v1
    registered_at: 2026-05-24T12:00:00Z
`)

	report, err := registry.BootstrapFromCerberus(ctx, svc, cerberusHome, false, true)
	if err != nil {
		t.Fatalf("BootstrapFromCerberus: %v", err)
	}
	if report.Attached != 1 || report.Imported != 0 {
		t.Fatalf("report = %+v; want attached=1 imported=0", report)
	}

	projects, err := svc.Search(ctx, registry.KindProject, registry.Filter{Status: registry.StatusAny})
	if err != nil {
		t.Fatalf("Search projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(projects))
	}
	if ext, ok := projects[0].ExternalIDFor("tether"); !ok || ext.ExternalID != "clockwork" {
		t.Fatalf("missing tether external id: %+v", projects[0].ExternalIDs)
	}
	if ext, ok := projects[0].ExternalIDFor("cerberus"); !ok || ext.ExternalID != "clockwork" {
		t.Fatalf("missing cerberus external id: %+v", projects[0].ExternalIDs)
	}

	b, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read cerberus project: %v", err)
	}
	if !strings.Contains(string(b), "registry_urn: "+projects[0].URN) {
		t.Fatalf("registry_urn write-back missing from %s:\n%s", projectPath, string(b))
	}
}

func TestBootstrapFromCerberus_WriteBackRejectsPathsOutsideHome(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	cerberusHome := t.TempDir()
	externalDir := t.TempDir()
	projectPath := writeBootstrapFile(t, externalDir, "clockwork.cerberus.yaml", `kind: cerberus-project/v1
owner: clockwork
namespace: local
project:
  id: clockwork
  name: Clockwork
resources: []
`)
	writeBootstrapFile(t, cerberusHome, "registry.yaml", `version: 1
entries:
  - owner: clockwork
    namespace: local
    path: `+projectPath+`
    kind: cerberus-project/v1
    registered_at: 2026-05-24T12:00:00Z
`)

	report, err := registry.BootstrapFromCerberus(ctx, svc, cerberusHome, false, true)
	if err != nil {
		t.Fatalf("BootstrapFromCerberus: %v", err)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("report.Errors = %d; want 1", len(report.Errors))
	}
	if !strings.Contains(report.Errors[0].Reason, "path escapes cerberus home") {
		t.Fatalf("error reason = %q; want path escape guard", report.Errors[0].Reason)
	}
}

func TestBootstrap_Idempotent_SecondRunSkipsAll(t *testing.T) {
	svc := newService(t)
	root := newCatalogRoot(t)
	ctx := context.Background()

	first, err := registry.BootstrapFromCatalog(ctx, svc, root, false)
	if err != nil {
		t.Fatalf("first BootstrapFromCatalog: %v", err)
	}
	if first.Imported != 4 {
		t.Fatalf("first run imported %d; want 4", first.Imported)
	}

	// Capture row identities before the second pass so we can verify
	// they're untouched.
	preAgents, _ := svc.Search(ctx, registry.KindAgent, registry.Filter{})
	preProjects, _ := svc.Search(ctx, registry.KindProject, registry.Filter{})
	preURNs := map[string]registry.Profile{}
	for _, p := range append(append([]registry.Profile{}, preAgents...), preProjects...) {
		preURNs[p.URN] = p
	}

	second, err := registry.BootstrapFromCatalog(ctx, svc, root, false)
	if err != nil {
		t.Fatalf("second BootstrapFromCatalog: %v", err)
	}
	if second.Imported != 0 {
		t.Errorf("second run Imported = %d; want 0", second.Imported)
	}
	if second.Skipped != 4 {
		t.Errorf("second run Skipped = %d; want 4", second.Skipped)
	}
	if second.Refreshed != 0 {
		t.Errorf("second run Refreshed = %d; want 0 on force=false", second.Refreshed)
	}

	// Row identities preserved.
	postAgents, _ := svc.Search(ctx, registry.KindAgent, registry.Filter{})
	postProjects, _ := svc.Search(ctx, registry.KindProject, registry.Filter{})
	for _, p := range append(append([]registry.Profile{}, postAgents...), postProjects...) {
		pre, ok := preURNs[p.URN]
		if !ok {
			t.Errorf("post-second-run URN %s did not exist before", p.URN)
			continue
		}
		// Skipped rows should not have their updated_at touched.
		if !p.UpdatedAt.Equal(pre.UpdatedAt) {
			t.Errorf("URN %s updated_at drifted: pre=%s post=%s",
				p.URN, pre.UpdatedAt, p.UpdatedAt)
		}
		if p.DisplayName != pre.DisplayName {
			t.Errorf("URN %s display_name drifted: pre=%q post=%q",
				p.URN, pre.DisplayName, p.DisplayName)
		}
	}
}

func TestBootstrap_Force_RefreshesEditedRow(t *testing.T) {
	root := t.TempDir()
	// Lone agent fixture so we can rewrite its YAML between passes.
	writeBootstrapFile(t, filepath.Join(root, "agents"), "alpha.yaml", `id: alpha
name: Alpha Original
roles: [implementer]
skills: [go]
`)

	// No file resolver needed — bootstrap's force=true path calls
	// UpdateSelf + BumpCachedAt directly rather than going through Sync
	// (Sync expects a Profile-shaped payload; bootstrap's source files
	// are in config.Agent shape).
	svc := newService(t)
	ctx := context.Background()

	first, err := svc.BootstrapFromCatalog(ctx, root, false)
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if first.Imported != 1 {
		t.Fatalf("first.Imported = %d; want 1", first.Imported)
	}

	pre, _ := svc.Search(ctx, registry.KindAgent, registry.Filter{})
	if len(pre) != 1 {
		t.Fatalf("pre-edit agent count = %d; want 1", len(pre))
	}
	preURN := pre[0].URN
	preDisplay := pre[0].DisplayName
	if preDisplay != "Alpha Original" {
		t.Fatalf("pre display_name = %q; want Alpha Original", preDisplay)
	}
	preCachedAt := pre[0].CachedAt

	// Mutate the YAML in place: rename and shuffle capabilities.
	// Sleep a tick to make sure any bumped timestamps clearly post-date
	// the original Register call.
	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "agents", "alpha.yaml"),
		[]byte(`id: alpha
name: Alpha Refreshed
roles: [implementer]
skills: [go, sqlite]
`), 0o600); err != nil {
		t.Fatalf("rewrite alpha.yaml: %v", err)
	}

	second, err := svc.BootstrapFromCatalog(ctx, root, true)
	if err != nil {
		t.Fatalf("force=true bootstrap: %v", err)
	}
	if second.Imported != 0 {
		t.Errorf("force=true Imported = %d; want 0", second.Imported)
	}
	if second.Refreshed != 1 {
		t.Errorf("force=true Refreshed = %d; want 1", second.Refreshed)
	}
	if second.Skipped != 0 {
		t.Errorf("force=true Skipped = %d; want 0", second.Skipped)
	}
	if len(second.Errors) != 0 {
		t.Errorf("force=true Errors = %+v; want empty", second.Errors)
	}

	// Verify the row was patched in place (same URN, refreshed fields).
	post, _ := svc.Lookup(ctx, preURN)
	if post.DisplayName != "Alpha Refreshed" {
		t.Errorf("post display_name = %q; want Alpha Refreshed", post.DisplayName)
	}
	if !equalUnordered(post.Capabilities, []string{"go", "sqlite"}) {
		t.Errorf("post capabilities = %v; want [go sqlite]", post.Capabilities)
	}
	if post.LastUpdatedBy != "system:bootstrap" {
		t.Errorf("post last_updated_by = %q; want system:bootstrap", post.LastUpdatedBy)
	}

	// cached_at must be set after force=true refresh — bootstrap calls
	// storage.BumpCachedAt explicitly after UpdateSelf. (The pre row has
	// cached_at unset since Register doesn't populate it.)
	if post.CachedAt == nil {
		t.Errorf("post cached_at is nil; expected force=true refresh to have stamped it")
	} else if preCachedAt != nil && !post.CachedAt.After(*preCachedAt) {
		t.Errorf("post cached_at = %s; expected to be after pre %s", post.CachedAt, preCachedAt)
	}
}

func TestBootstrap_BackupFiles_Excluded(t *testing.T) {
	svc := newService(t)
	root := t.TempDir()

	// One valid agent + several variations of backup names.
	writeBootstrapFile(t, filepath.Join(root, "agents"), "alpha.yaml", `name: Real Alpha
roles: [r]
`)
	writeBootstrapFile(t, filepath.Join(root, "agents"), "alpha.yaml.bak-2026", `name: STALE-1`)
	writeBootstrapFile(t, filepath.Join(root, "agents"), "alpha.yaml.bak-old.yaml", `name: STALE-2`)

	report, err := registry.BootstrapFromCatalog(context.Background(), svc, root, false)
	if err != nil {
		t.Fatalf("BootstrapFromCatalog: %v", err)
	}
	if report.Imported != 1 {
		t.Errorf("Imported = %d; want 1 (backup files excluded)", report.Imported)
	}
	if len(report.Errors) != 0 {
		t.Errorf("Errors = %+v; want empty (backup files are silent skips)", report.Errors)
	}

	agents, _ := svc.Search(context.Background(), registry.KindAgent, registry.Filter{})
	for _, a := range agents {
		if strings.Contains(a.DisplayName, "STALE") {
			t.Errorf("backup leaked into registry: %+v", a)
		}
	}
}

func TestBootstrap_MissingSubdir_TreatedAsEmpty(t *testing.T) {
	svc := newService(t)
	root := t.TempDir()

	// Only agents/ — no projects/ directory at all.
	writeBootstrapFile(t, filepath.Join(root, "agents"), "alpha.yaml", `name: A
roles: [r]
`)

	report, err := registry.BootstrapFromCatalog(context.Background(), svc, root, false)
	if err != nil {
		t.Fatalf("BootstrapFromCatalog: %v", err)
	}
	if report.Imported != 1 {
		t.Errorf("Imported = %d; want 1 (1 agent, 0 projects)", report.Imported)
	}
	if len(report.Errors) != 0 {
		t.Errorf("Errors = %+v; want empty (missing projects/ is normal)", report.Errors)
	}
}

func TestBootstrap_SkipOnError_DoesNotAbort(t *testing.T) {
	svc := newService(t)
	root := t.TempDir()

	// Three agents: two valid sandwiching a malformed file. Sorted scan
	// means we hit them in {agents/a.yaml, agents/bad.yaml, agents/c.yaml}
	// order, which deliberately exercises "error in the middle".
	writeBootstrapFile(t, filepath.Join(root, "agents"), "a.yaml", `name: Aye
roles: [r]
`)
	writeBootstrapFile(t, filepath.Join(root, "agents"), "bad.yaml", "name: [unbalanced\n")
	writeBootstrapFile(t, filepath.Join(root, "agents"), "c.yaml", `name: See
roles: [r]
`)

	report, err := registry.BootstrapFromCatalog(context.Background(), svc, root, false)
	if err != nil {
		t.Fatalf("BootstrapFromCatalog: %v", err)
	}
	if report.Imported != 2 {
		t.Errorf("Imported = %d; want 2 (a.yaml + c.yaml)", report.Imported)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("Errors = %d; want 1 (bad.yaml). Got: %+v", len(report.Errors), report.Errors)
	}
	if !strings.Contains(report.Errors[0].Reason, "parse") {
		t.Errorf("Errors[0].Reason = %q; want a parse-yaml flavor", report.Errors[0].Reason)
	}
}

func TestBootstrap_ServiceWrapper_DelegatesToPackage(t *testing.T) {
	svc := newService(t)
	root := newCatalogRoot(t)

	r1, err := registry.BootstrapFromCatalog(context.Background(), svc, root, false)
	if err != nil {
		t.Fatalf("package-level call: %v", err)
	}
	// Same root, same service → idempotent second pass via the wrapper.
	r2, err := svc.BootstrapFromCatalog(context.Background(), root, false)
	if err != nil {
		t.Fatalf("Service.BootstrapFromCatalog: %v", err)
	}
	if r2.Imported != 0 || r2.Skipped != r1.Imported {
		t.Errorf("wrapper not delegating: r1=%+v r2=%+v", r1, r2)
	}
}

func TestBootstrap_NilService(t *testing.T) {
	if _, err := registry.BootstrapFromCatalog(context.Background(), nil, "/tmp/anything", false); err == nil {
		t.Fatalf("expected error for nil service")
	}
}

func TestBootstrap_EmptyCatalogRoot(t *testing.T) {
	svc := newService(t)
	if _, err := registry.BootstrapFromCatalog(context.Background(), svc, "", false); err == nil {
		t.Fatalf("expected error for empty catalog root")
	}
}

// equalUnordered reports whether a and b have the same elements
// regardless of order. Used because storage layer orders capabilities
// alphabetically on read, which is a fine guarantee for this package
// but tests should be insensitive to it.
func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// Silence unused-import linting if errors goes away during refactors.
var _ = errors.Is
