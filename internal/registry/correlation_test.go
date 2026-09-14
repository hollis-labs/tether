package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func TestProfile_DerivedVsAuthored(t *testing.T) {
	now := time.Now().UTC()
	cached := now.Add(-5 * time.Minute)
	p := registry.Profile{
		URN:           "msg://project/agent-mux/prj_test",
		Kind:          registry.KindProject,
		DisplayName:   "Test Project",
		Description:   "A test project description",
		Tags:          []string{"backend", "go"},
		Guidelines:    "Follow standard Go conventions.",
		EntryPoints:   []string{"cmd/mux/main.go"},
		LastUpdatedBy: "author:test",
		CreatedAt:     now,
		UpdatedAt:     now,
		CachedAt:      &cached,
		FieldMetadata: map[string]registry.FieldMeta{
			"description": {
				Class:         registry.FieldClassAuthored,
				LastUpdatedBy: "author:test",
				UpdatedAt:     now,
			},
			"tags": {
				Class:         registry.FieldClassAuthored,
				LastUpdatedBy: "author:test",
				UpdatedAt:     now,
			},
			"guidelines": {
				Class:         registry.FieldClassAuthored,
				LastUpdatedBy: "author:test",
				UpdatedAt:     now,
			},
			"entry_points": {
				Class:         registry.FieldClassAuthored,
				LastUpdatedBy: "author:test",
				UpdatedAt:     now,
			},
			"display_name": {
				Class:         registry.FieldClassDerived,
				LastUpdatedBy: "system:sync",
				UpdatedAt:     now,
				CachedAt:      &cached,
			},
		},
	}

	// Authored projection
	auth := p.Authored()
	if auth.Description != p.Description {
		t.Fatalf("auth.Description = %q; want %q", auth.Description, p.Description)
	}
	if len(auth.Tags) != 2 || auth.Tags[0] != "backend" || auth.Tags[1] != "go" {
		t.Fatalf("auth.Tags = %v; want [backend go]", auth.Tags)
	}
	if auth.Guidelines != p.Guidelines {
		t.Fatalf("auth.Guidelines = %q; want %q", auth.Guidelines, p.Guidelines)
	}
	if len(auth.EntryPoints) != 1 || auth.EntryPoints[0] != "cmd/mux/main.go" {
		t.Fatalf("auth.EntryPoints = %v; want [cmd/mux/main.go]", auth.EntryPoints)
	}

	// Derived projection
	der := p.Derived()
	if der.URN != p.URN || der.Kind != p.Kind || der.DisplayName != p.DisplayName {
		t.Fatalf("der.DisplayName = %q, want %q", der.DisplayName, p.DisplayName)
	}

	// Field classification
	for _, field := range []string{"description", "tags", "guidelines", "entry_points"} {
		if got := p.FieldClassFor(field); got != registry.FieldClassAuthored {
			t.Errorf("p.FieldClassFor(%q) = %q; want authored", field, got)
		}
	}
	for _, field := range []string{"urn", "kind", "display_name", "callback", "status", "mux_instance_id"} {
		if got := p.FieldClassFor(field); got != registry.FieldClassDerived {
			t.Errorf("p.FieldClassFor(%q) = %q; want derived", field, got)
		}
	}

	// Provenance & Freshness
	metaTags, ok := p.FieldMetaFor("tags")
	if !ok || metaTags.Class != registry.FieldClassAuthored || metaTags.LastUpdatedBy != "author:test" {
		t.Errorf("metaTags = %+v, ok=%v; want author:test", metaTags, ok)
	}
	metaDisp, ok := p.FieldMetaFor("display_name")
	if !ok || metaDisp.Class != registry.FieldClassDerived || metaDisp.CachedAt == nil || !metaDisp.CachedAt.Equal(cached) {
		t.Errorf("metaDisp = %+v, ok=%v; want cached_at %v", metaDisp, ok, cached)
	}
}

func TestStorage_CorrelationFields_RoundtripAndSearch(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	now := time.Now().UTC()
	cached := now.Add(-1 * time.Minute)
	p := registry.Profile{
		Kind:          registry.KindProject,
		DisplayName:   "Tether Daemon",
		Description:   "Agent control plane",
		Tags:          []string{"go", "daemon", "infrastructure"},
		Guidelines:    "Review ADRs before modifying storage.",
		EntryPoints:   []string{"cmd/mux/root.go", "internal/app/app.go"},
		LastUpdatedBy: "author:dev",
		CachedAt:      &cached,
	}

	registered, err := svc.Register(ctx, registry.KindProject, p)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if len(registered.Tags) != 3 {
		t.Fatalf("registered.Tags = %v; want 3 items", registered.Tags)
	}
	if registered.Guidelines != "Review ADRs before modifying storage." {
		t.Fatalf("registered.Guidelines = %q", registered.Guidelines)
	}
	if len(registered.EntryPoints) != 2 {
		t.Fatalf("registered.EntryPoints = %v; want 2 items", registered.EntryPoints)
	}
	if len(registered.FieldMetadata) == 0 {
		t.Fatalf("registered.FieldMetadata is empty; expected auto-synthesized metadata")
	}

	// Verify lookup preserves all correlation fields
	got, err := svc.Lookup(ctx, registered.URN)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Guidelines != registered.Guidelines {
		t.Errorf("got.Guidelines = %q; want %q", got.Guidelines, registered.Guidelines)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "go" {
		t.Errorf("got.Tags = %v; want %v", got.Tags, registered.Tags)
	}
	if len(got.EntryPoints) != 2 || got.EntryPoints[0] != "cmd/mux/root.go" {
		t.Errorf("got.EntryPoints = %v; want %v", got.EntryPoints, registered.EntryPoints)
	}

	// Search by Tag
	matches, err := svc.Search(ctx, registry.KindProject, registry.Filter{Tag: "daemon"})
	if err != nil {
		t.Fatalf("Search tag=daemon: %v", err)
	}
	if len(matches) != 1 || matches[0].URN != registered.URN {
		t.Fatalf("Search tag=daemon returned %v; want [%s]", matches, registered.URN)
	}

	noMatches, err := svc.Search(ctx, registry.KindProject, registry.Filter{Tag: "frontend"})
	if err != nil {
		t.Fatalf("Search tag=frontend: %v", err)
	}
	if len(noMatches) != 0 {
		t.Fatalf("Search tag=frontend returned %v; want empty", noMatches)
	}
}

func TestService_UpdateSelf_CorrelationFields(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	p := registry.Profile{
		Kind:          registry.KindProject,
		DisplayName:   "Correlation Target",
		Description:   "Original description",
		Tags:          []string{"v1", "shared"},
		Guidelines:    "Original guidelines",
		EntryPoints:   []string{"main.go"},
		LastUpdatedBy: "author:init",
	}

	created, err := svc.Register(ctx, registry.KindProject, p)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 1. Update Guidelines
	newGuidelines := "Updated guidelines for v2"
	updated, err := svc.UpdateSelf(ctx, created.URN, registry.UpdatePatch{
		Guidelines:    &newGuidelines,
		LastUpdatedBy: "author:editor",
	})
	if err != nil {
		t.Fatalf("UpdateSelf guidelines: %v", err)
	}
	if updated.Guidelines != newGuidelines {
		t.Errorf("updated.Guidelines = %q; want %q", updated.Guidelines, newGuidelines)
	}
	metaG, ok := updated.FieldMetaFor("guidelines")
	if !ok || metaG.Class != registry.FieldClassAuthored || metaG.LastUpdatedBy != "author:editor" {
		t.Errorf("metaG = %+v, ok=%v; want author:editor", metaG, ok)
	}

	// 2. Append Tags
	updated, err = svc.UpdateSelf(ctx, created.URN, registry.UpdatePatch{
		Tags: &registry.ArrayPatch[string]{
			Mode:  registry.ArrayModeAppend,
			Value: []string{"v2", "beta"},
		},
		LastUpdatedBy: "author:tagger",
	})
	if err != nil {
		t.Fatalf("UpdateSelf tags append: %v", err)
	}
	if len(updated.Tags) != 4 {
		t.Errorf("updated.Tags = %v; want 4 elements", updated.Tags)
	}

	// 3. Remove Tag
	updated, err = svc.UpdateSelf(ctx, created.URN, registry.UpdatePatch{
		Tags: &registry.ArrayPatch[string]{
			Mode:  registry.ArrayModeRemove,
			Value: []string{"v1"},
		},
		LastUpdatedBy: "author:tagger",
	})
	if err != nil {
		t.Fatalf("UpdateSelf tags remove: %v", err)
	}
	if len(updated.Tags) != 3 {
		t.Errorf("updated.Tags = %v; want 3 elements after removing v1", updated.Tags)
	}

	// 4. Replace EntryPoints
	updated, err = svc.UpdateSelf(ctx, created.URN, registry.UpdatePatch{
		EntryPoints: &registry.ArrayPatch[string]{
			Mode:  registry.ArrayModeReplace,
			Value: []string{"entry.go", "cli.go"},
		},
		LastUpdatedBy: "author:coder",
	})
	if err != nil {
		t.Fatalf("UpdateSelf entry_points replace: %v", err)
	}
	if len(updated.EntryPoints) != 2 || updated.EntryPoints[0] != "entry.go" {
		t.Errorf("updated.EntryPoints = %v; want [entry.go cli.go]", updated.EntryPoints)
	}
	metaEP, ok := updated.FieldMetaFor("entry_points")
	if !ok || metaEP.Class != registry.FieldClassAuthored || metaEP.LastUpdatedBy != "author:coder" {
		t.Errorf("metaEP = %+v, ok=%v; want author:coder", metaEP, ok)
	}
}

func TestService_Merge_CorrelationFields(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)

	src, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		Kind:          registry.KindProject,
		DisplayName:   "Source Project",
		Description:   "Source description",
		Tags:          []string{"tag-a", "shared-tag"},
		Guidelines:    "Source guidelines",
		EntryPoints:   []string{"src/main.go"},
		LastUpdatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("Register src: %v", err)
	}

	dst, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		Kind:          registry.KindProject,
		DisplayName:   "Dest Project",
		Tags:          []string{"tag-b", "shared-tag"},
		EntryPoints:   []string{"cmd/app.go"},
		LastUpdatedBy: "tester",
	})
	if err != nil {
		t.Fatalf("Register dst: %v", err)
	}

	merged, err := svc.Merge(ctx, src.URN, dst.URN)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Tags should be unioned
	if len(merged.Tags) != 3 {
		t.Errorf("merged.Tags = %v; want 3 unique tags", merged.Tags)
	}

	// EntryPoints should be unioned
	if len(merged.EntryPoints) != 2 {
		t.Errorf("merged.EntryPoints = %v; want 2 unique entry points", merged.EntryPoints)
	}

	// Empty dst guidelines/description should adopt non-empty src values
	if merged.Guidelines != "Source guidelines" {
		t.Errorf("merged.Guidelines = %q; want %q", merged.Guidelines, "Source guidelines")
	}
	if merged.Description != "Source description" {
		t.Errorf("merged.Description = %q; want %q", merged.Description, "Source description")
	}
}

func TestBackfillFieldMetadata(t *testing.T) {
	ctx := context.Background()
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage)

	// Simulate pre-migration rows inserted directly into the DB with NULL field_metadata_json
	_, err := db.ExecContext(ctx, `
		INSERT INTO registry_entries
			(urn, kind, display_name, description, tags_json, guidelines, entry_points_json, status, created_at, updated_at)
		VALUES
			('msg://project/agent-mux/prj_legacy', 'project', 'Legacy Project', 'Old desc', '["legacy","db"]', 'Old rules', '["legacy.go"]', 'active', '2026-05-01T00:00:00Z', '2026-05-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Verify legacy row has empty FieldMetadata
	legacy, err := svc.Lookup(ctx, "msg://project/agent-mux/prj_legacy")
	if err != nil {
		t.Fatalf("Lookup legacy: %v", err)
	}
	if len(legacy.FieldMetadata) != 0 {
		t.Fatalf("legacy.FieldMetadata = %v; want empty before backfill", legacy.FieldMetadata)
	}

	// Run BackfillFieldMetadata
	count, err := svc.BackfillFieldMetadata(ctx)
	if err != nil {
		t.Fatalf("BackfillFieldMetadata: %v", err)
	}
	if count < 1 {
		t.Fatalf("BackfillFieldMetadata returned count = %d; want >= 1", count)
	}

	// Verify row now has synthesized FieldMetadata
	backfilled, err := svc.Lookup(ctx, "msg://project/agent-mux/prj_legacy")
	if err != nil {
		t.Fatalf("Lookup backfilled: %v", err)
	}
	if len(backfilled.FieldMetadata) == 0 {
		t.Fatalf("backfilled.FieldMetadata is still empty")
	}
	if backfilled.FieldClassFor("guidelines") != registry.FieldClassAuthored {
		t.Errorf("FieldClassFor(guidelines) = %q; want authored", backfilled.FieldClassFor("guidelines"))
	}
	if backfilled.FieldClassFor("display_name") != registry.FieldClassDerived {
		t.Errorf("FieldClassFor(display_name) = %q; want derived", backfilled.FieldClassFor("display_name"))
	}

	// Second run should be a no-op
	count2, err := svc.BackfillFieldMetadata(ctx)
	if err != nil {
		t.Fatalf("Second BackfillFieldMetadata: %v", err)
	}
	if count2 != 0 {
		t.Errorf("Second BackfillFieldMetadata count = %d; want 0", count2)
	}
}

func TestService_Sync_NeverTouchesAuthoredFields(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sync_payload.json")
	payload := []byte(`{
		"display_name": "Synced Derived Name",
		"project": "synced-project",
		"health_status": "degraded",
		"host_address": "10.0.0.1:9090",
		"description": "Attempted overwrite description",
		"tags": ["overwritten-tag"],
		"guidelines": "Attempted overwrite guidelines",
		"entry_points": ["overwritten.go"],
		"title": "Attempted Overwrite Title",
		"role": "Attempted Overwrite Role",
		"avatar": "https://overwritten.invalid/avatar.png",
		"capabilities": ["overwritten-cap"],
		"skills": [{"name": "overwritten-skill"}],
		"links": [{"kind": "bad", "target": "https://bad.invalid"}]
	}`)
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatalf("write payload fixture: %v", err)
	}

	resolver, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage, registry.WithResolver(resolver))
	ctx := context.Background()

	skillTime := time.Now().UTC().Truncate(time.Second)
	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName:  "Original Name",
		Project:      "original-project",
		HealthStatus: "healthy",
		HostAddress:  "127.0.0.1:8080",
		Description:  "Original authored description",
		Tags:         []string{"backend", "go"},
		Guidelines:   "Original authored guidelines",
		EntryPoints:  []string{"cmd/app/main.go"},
		Title:        "Staff Engineer",
		Role:         "Tech Lead",
		Avatar:       "https://example.com/avatar.png",
		Capabilities: []string{"golang", "sqlite"},
		Skills: []registry.Skill{
			{Name: "debugging", LearnedAt: skillTime},
		},
		Links: []registry.Link{
			{Kind: "repo", Target: "https://github.com/hollis-labs/tether"},
		},
		Callback: &registry.Callback{
			Scheme: "file",
			Target: "file://" + target,
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Sync against the payload that specifies conflicting values for ALL fields.
	synced, err := svc.Sync(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// 1. Derived fields MUST update to the new values
	if synced.DisplayName != "Synced Derived Name" {
		t.Errorf("DisplayName = %q; want %q", synced.DisplayName, "Synced Derived Name")
	}
	if synced.Project != "synced-project" {
		t.Errorf("Project = %q; want %q", synced.Project, "synced-project")
	}
	if synced.HealthStatus != "degraded" {
		t.Errorf("HealthStatus = %q; want %q", synced.HealthStatus, "degraded")
	}
	if synced.HostAddress != "10.0.0.1:9090" {
		t.Errorf("HostAddress = %q; want %q", synced.HostAddress, "10.0.0.1:9090")
	}

	// 2. Authored fields MUST remain byte-for-byte untouched (CW-20260912-0095 Scope Item 3)
	if synced.Description != "Original authored description" {
		t.Errorf("Description = %q; want %q", synced.Description, "Original authored description")
	}
	if !reflect.DeepEqual(synced.Tags, []string{"backend", "go"}) {
		t.Errorf("Tags = %v; want [backend go]", synced.Tags)
	}
	if synced.Guidelines != "Original authored guidelines" {
		t.Errorf("Guidelines = %q; want %q", synced.Guidelines, "Original authored guidelines")
	}
	if !reflect.DeepEqual(synced.EntryPoints, []string{"cmd/app/main.go"}) {
		t.Errorf("EntryPoints = %v; want [cmd/app/main.go]", synced.EntryPoints)
	}
	if synced.Title != "Staff Engineer" {
		t.Errorf("Title = %q; want %q", synced.Title, "Staff Engineer")
	}
	if synced.Role != "Tech Lead" {
		t.Errorf("Role = %q; want %q", synced.Role, "Tech Lead")
	}
	if synced.Avatar != "https://example.com/avatar.png" {
		t.Errorf("Avatar = %q; want %q", synced.Avatar, "https://example.com/avatar.png")
	}
	if !reflect.DeepEqual(synced.Capabilities, []string{"golang", "sqlite"}) {
		t.Errorf("Capabilities = %v; want [golang sqlite]", synced.Capabilities)
	}
	if len(synced.Skills) != 1 || synced.Skills[0].Name != "debugging" {
		t.Errorf("Skills = %v; want [{debugging}]", synced.Skills)
	}
	if len(synced.Links) != 1 || synced.Links[0].Kind != "repo" || synced.Links[0].Target != "https://github.com/hollis-labs/tether" {
		t.Errorf("Links = %v; want repo link", synced.Links)
	}

	// 3. Stamped freshness: CachedAt row-level bumped, and per-field CachedAt stamped ONLY on touched derived fields
	if synced.CachedAt == nil {
		t.Fatal("synced.CachedAt is nil; want bumped value")
	}
	for _, derivedField := range []string{"display_name", "project", "health_status", "host_address"} {
		meta, ok := synced.FieldMetaFor(derivedField)
		if !ok {
			t.Errorf("missing FieldMetadata for derived field %q", derivedField)
			continue
		}
		if meta.Class != registry.FieldClassDerived {
			t.Errorf("FieldMetadata[%q].Class = %q; want derived", derivedField, meta.Class)
		}
		if meta.CachedAt == nil || !meta.CachedAt.Equal(*synced.CachedAt) {
			t.Errorf("FieldMetadata[%q].CachedAt = %v; want %v", derivedField, meta.CachedAt, synced.CachedAt)
		}
		if meta.LastUpdatedBy != "system:sync" {
			t.Errorf("FieldMetadata[%q].LastUpdatedBy = %q; want system:sync", derivedField, meta.LastUpdatedBy)
		}
	}
	for _, authoredField := range []string{"description", "tags", "guidelines", "entry_points", "title", "role", "avatar", "capabilities"} {
		meta, ok := synced.FieldMetaFor(authoredField)
		if ok && meta.CachedAt != nil {
			t.Errorf("authored field %q unexpectedly has CachedAt stamped: %v", authoredField, meta.CachedAt)
		}
	}
}

func TestService_Sync_ResolverFailureLeavesLastGood(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sync_target.json")
	if err := os.WriteFile(target, []byte(`{"display_name": "Initial Good Name", "health_status": "healthy"}`), 0o644); err != nil {
		t.Fatalf("write initial fixture: %v", err)
	}

	resolver, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage, registry.WithResolver(resolver))
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Initial Register Name",
		Description: "Important authored note",
		Role:        "specialist",
		Callback: &registry.Callback{
			Scheme: "file",
			Target: "file://" + target,
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 1. First sync succeeds, establishing the initial good state
	lastGood, err := svc.Sync(ctx, reg.URN)
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if lastGood.DisplayName != "Initial Good Name" {
		t.Fatalf("first Sync DisplayName = %q; want Initial Good Name", lastGood.DisplayName)
	}
	if lastGood.CachedAt == nil {
		t.Fatal("first Sync CachedAt is nil")
	}

	// 2. Break the fixture (corrupt JSON)
	if err := os.WriteFile(target, []byte(`{INVALID JSON`), 0o644); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}

	// 3. Second sync must fail and report the failure (must not pretend success)
	_, syncErr := svc.Sync(ctx, reg.URN)
	if syncErr == nil {
		t.Fatal("second Sync succeeded; want error on corrupt payload")
	}

	// 4. Stored state must remain completely unchanged (last good value intact, not blanked)
	reloaded, err := svc.Lookup(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Lookup after failed sync: %v", err)
	}
	if reloaded.DisplayName != lastGood.DisplayName {
		t.Errorf("DisplayName mutated on failure: got %q, want %q", reloaded.DisplayName, lastGood.DisplayName)
	}
	if reloaded.HealthStatus != lastGood.HealthStatus {
		t.Errorf("HealthStatus mutated on failure: got %q, want %q", reloaded.HealthStatus, lastGood.HealthStatus)
	}
	if reloaded.Description != lastGood.Description {
		t.Errorf("Description mutated on failure: got %q, want %q", reloaded.Description, lastGood.Description)
	}
	if reloaded.Role != lastGood.Role {
		t.Errorf("Role mutated on failure: got %q, want %q", reloaded.Role, lastGood.Role)
	}
	if !reloaded.CachedAt.Equal(*lastGood.CachedAt) {
		t.Errorf("CachedAt changed on failed sync: got %v, want %v", reloaded.CachedAt, lastGood.CachedAt)
	}
}

func TestBootstrap_IdentityKeyedIdempotency(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	// Two project files in different catalog paths representing the SAME project id
	writeBootstrapFile(t, filepath.Join(root, "projects"), "clockwork_path_a.yaml", `id: clockwork
name: Clockwork Project
repo_root: /repos/clockwork
tracking_root: /tracking/clockwork
`)
	writeBootstrapFile(t, filepath.Join(root, "projects"), "clockwork_path_b.yaml", `id: clockwork
name: Clockwork Project Renamed
repo_root: /repos/clockwork
tracking_root: /tracking/clockwork
`)

	report, err := registry.BootstrapFromCatalog(ctx, svc, root, false)
	if err != nil {
		t.Fatalf("BootstrapFromCatalog: %v", err)
	}

	// Exactly 1 project should be imported, 1 skipped because of matching external ID
	if report.Imported != 1 {
		t.Errorf("report.Imported = %d; want 1", report.Imported)
	}
	if report.Skipped != 1 {
		t.Errorf("report.Skipped = %d; want 1", report.Skipped)
	}

	projects, err := svc.Search(ctx, registry.KindProject, registry.Filter{Status: registry.StatusAny})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects count = %d; want exactly 1 project row", len(projects))
	}

	// Look up by external ID resolves to this single row
	lookedUp, err := svc.LookupBy(ctx, registry.KindProject, "clockwork", "tether")
	if err != nil {
		t.Fatalf("LookupBy: %v", err)
	}
	if lookedUp.URN != projects[0].URN {
		t.Errorf("LookupBy URN = %q; want %q", lookedUp.URN, projects[0].URN)
	}
}

func TestSharedProject_C3_ServiceOnboardingTorqueAndPartialCoverage(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	// 1. Onboarding: Register project with torque and tether external IDs
	proj, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Tether Shared Project",
		Description: "Federated cross-app control plane",
		Guidelines:  "Follow AGENTS.md conventions",
		Tags:        []string{"control-plane", "federation"},
		ExternalIDs: []registry.ExternalID{
			{Substrate: "tether", ExternalID: "tether-core"},
			{Substrate: "torque", ExternalID: "PRJ-TETHER-100"},
		},
		Callback: &registry.Callback{
			Scheme: "cli",
			Target: "mux describe --json",
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 2. Resolve: Verify AttachedAt is defaulted and valid, and external IDs are returned
	loaded, err := svc.Lookup(ctx, proj.URN)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(loaded.ExternalIDs) != 2 {
		t.Fatalf("ExternalIDs count = %d, want 2", len(loaded.ExternalIDs))
	}
	for _, ext := range loaded.ExternalIDs {
		if ext.AttachedAt.IsZero() || ext.AttachedAt.Year() <= 1 {
			t.Errorf("substrate %s attached_at is zero/invalid: %v", ext.Substrate, ext.AttachedAt)
		}
	}

	extsDirect, err := svc.LookupExternalIDsForURN(ctx, proj.URN)
	if err != nil {
		t.Fatalf("LookupExternalIDsForURN: %v", err)
	}
	if len(extsDirect) != 2 {
		t.Fatalf("LookupExternalIDsForURN count = %d, want 2", len(extsDirect))
	}

	// 3. Reverse: Look up by torque external ID
	byTorque, err := svc.LookupBy(ctx, registry.KindProject, "PRJ-TETHER-100", "torque")
	if err != nil {
		t.Fatalf("LookupBy torque: %v", err)
	}
	if byTorque.URN != proj.URN {
		t.Errorf("LookupBy torque URN = %q, want %q", byTorque.URN, proj.URN)
	}

	// 4. Reverse: Look up by tether external ID
	byTether, err := svc.LookupBy(ctx, registry.KindProject, "tether-core", "tether")
	if err != nil {
		t.Fatalf("LookupBy tether: %v", err)
	}
	if byTether.URN != proj.URN {
		t.Errorf("LookupBy tether URN = %q, want %q", byTether.URN, proj.URN)
	}

	// 5. Partial-coverage discipline:
	// A project with only cerberus substrate ID is completely valid and complete.
	partProj, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Cerberus Only Project",
		ExternalIDs: []registry.ExternalID{
			{Substrate: "cerberus", ExternalID: "cerb-only"},
		},
	})
	if err != nil {
		t.Fatalf("Register partial: %v", err)
	}

	partLoaded, err := svc.Lookup(ctx, partProj.URN)
	if err != nil {
		t.Fatalf("Lookup partial: %v", err)
	}
	if len(partLoaded.ExternalIDs) != 1 || partLoaded.ExternalIDs[0].Substrate != "cerberus" {
		t.Errorf("partial ExternalIDs = %+v, want only cerberus", partLoaded.ExternalIDs)
	}

	// Reverse lookup on torque for missing ID returns ErrNotFound cleanly
	_, errNotFound := svc.LookupBy(ctx, registry.KindProject, "PRJ-NONEXISTENT", "torque")
	if !errors.Is(errNotFound, registry.ErrNotFound) {
		t.Errorf("LookupBy missing ID error = %v, want ErrNotFound", errNotFound)
	}

	// 6. Idempotent key registration with existing key returns existing row
	idempOut, created, err := svc.RegisterIdempotent(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Tether Shared Project Re-attempt",
	}, "torque", "PRJ-TETHER-100")
	if err != nil {
		t.Fatalf("RegisterIdempotent: %v", err)
	}
	if created {
		t.Error("expected created=false on repeat idempotent registration")
	}
	if idempOut.URN != proj.URN {
		t.Errorf("RegisterIdempotent URN = %q, want %q", idempOut.URN, proj.URN)
	}

	// 7. Offboarding terminal state (Deregister):
	dereg, err := svc.Deregister(ctx, partProj.URN)
	if err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if dereg.Status != registry.StatusDeprecated {
		t.Errorf("dereg status = %q, want deprecated", dereg.Status)
	}

	// Default search excludes deprecated rows
	activeRows, err := svc.Search(ctx, registry.KindProject, registry.Filter{})
	if err != nil {
		t.Fatalf("Search active: %v", err)
	}
	for _, row := range activeRows {
		if row.URN == partProj.URN {
			t.Errorf("default search included deprecated row %s", partProj.URN)
		}
	}

	// Deprecated row is still directly resolvable by URN
	deregLoaded, err := svc.Lookup(ctx, partProj.URN)
	if err != nil {
		t.Fatalf("Lookup deprecated: %v", err)
	}
	if deregLoaded.Status != registry.StatusDeprecated {
		t.Errorf("Lookup deprecated status = %q, want deprecated", deregLoaded.Status)
	}

	// 8. Offboarding terminal state (Merge):
	dupProj, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Duplicate Tether",
		ExternalIDs: []registry.ExternalID{
			{Substrate: "cerberus", ExternalID: "PRJ-DUP-CERBERUS"},
		},
	})
	if err != nil {
		t.Fatalf("Register duplicate: %v", err)
	}
	mergedDst, err := svc.Merge(ctx, dupProj.URN, proj.URN)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if mergedDst.URN != proj.URN {
		t.Errorf("Merge dst URN = %q, want %q", mergedDst.URN, proj.URN)
	}

	// PRJ-DUP-CERBERUS now resolves to proj.URN
	byMergedCerb, err := svc.LookupBy(ctx, registry.KindProject, "PRJ-DUP-CERBERUS", "cerberus")
	if err != nil {
		t.Fatalf("LookupBy merged cerberus: %v", err)
	}
	if byMergedCerb.URN != proj.URN {
		t.Errorf("LookupBy merged cerberus URN = %q, want %q", byMergedCerb.URN, proj.URN)
	}
}
