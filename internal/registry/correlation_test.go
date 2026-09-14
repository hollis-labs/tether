package registry_test

import (
	"context"
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
