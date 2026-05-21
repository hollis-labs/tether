package registry_test

// storage_test.go — coverage for storage.go (T-v060-01-02). The matrix:
//
//   - InsertProfile + GetProfile round-trip with all optional fields set
//     (Callback, KindMeta, capabilities, skills, links).
//   - GetProfile ErrNotFound on absent URN.
//   - UpdateProfileFields scalar update, updated_at bump, ErrUnknownColumn
//     rejection (including immutable columns).
//   - ReplaceCapabilities/Skills/Links: replacement is atomic; transient
//     PK conflict mid-tx rolls back without partial state.
//   - AppendCapabilities/Skills/Links: PK conflicts are silent dedup;
//     skills re-append does not refresh learned_at.
//   - RemoveCapabilities/Skills/Links: matches specific rows; empty input
//     is a no-op (no updated_at bump).
//   - SoftDelete: status flips, updated_at bumps, child tables UNTOUCHED.
//     This is the load-bearing amended T-02 acceptance criterion.
//   - Search: each scalar filter alone; capability + skill_name filters;
//     combined filters; alphabetical ordering; default excludes deprecated;
//     explicit deprecated returns deprecated only; StatusAny returns both.
//   - BumpCachedAt: cached_at + updated_at both bump.
//   - URNExists: true for present, false for absent.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// newStorage builds an in-memory SQLite with the registry migration applied
// and returns the storage wrapper. Reuses openInMemory from schema_test.go.
func newStorage(t *testing.T) *registry.Storage {
	t.Helper()
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return registry.NewStorage(db)
}

func fixedTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse fixed time %q: %v", s, err)
	}
	return ts
}

// sampleProfile returns a Profile with every optional field populated.
func sampleProfile(t *testing.T, urn string) registry.Profile {
	t.Helper()
	created := fixedTime(t, "2026-05-20T10:00:00Z")
	cached := fixedTime(t, "2026-05-20T11:00:00Z")
	lastSeen := fixedTime(t, "2026-05-20T11:30:00Z")
	learned := fixedTime(t, "2026-05-15T09:00:00Z")
	return registry.Profile{
		URN:           urn,
		Kind:          registry.KindAgent,
		MuxInstanceID: "agent-mux",
		DisplayName:   "Sprint Agent Alpha",
		Title:         "Implementer",
		Role:          "implementer",
		Description:   "Drives the v060-01 sprint home.",
		Avatar:        "https://example.invalid/avatar.png",
		Project:       "tether",
		Status:        registry.StatusActive,
		Callback: &registry.Callback{
			Scheme: "file",
			Target: "file:///tmp/agent.yaml",
		},
		CachedAt:      &cached,
		HealthStatus:  "ok",
		LastSeenAt:    &lastSeen,
		HostAddress:   "127.0.0.1:9000",
		KindMeta:      json.RawMessage(`{"roles":["secondary"],"source_path":"/tmp/a.yaml"}`),
		LastUpdatedBy: "bootstrap",
		Capabilities:  []string{"go", "sqlite"},
		Skills: []registry.Skill{
			{Name: "registry-design", LearnedAt: learned, Via: "sprint v060-01", Level: "expert"},
			{Name: "schema-migration", LearnedAt: learned, Via: "", Level: ""},
		},
		Links: []registry.Link{
			{Kind: "primary_mailbox", Target: "msg://agent/agent-mux/tether-sprint-1-implementer"},
			{Kind: "repo", Target: "https://example.invalid/tether"},
		},
		CreatedAt: created,
		UpdatedAt: created,
	}
}

func TestStorage_InsertGet_RoundTrip(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_round12345"

	want := sampleProfile(t, urn)
	if err := st.InsertProfile(ctx, want); err != nil {
		t.Fatalf("InsertProfile: %v", err)
	}

	got, err := st.GetProfile(ctx, urn)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}

	if got.URN != want.URN || got.Kind != want.Kind || got.MuxInstanceID != want.MuxInstanceID {
		t.Errorf("identity mismatch: got %+v want %+v", got, want)
	}
	if got.DisplayName != want.DisplayName || got.Title != want.Title || got.Role != want.Role {
		t.Errorf("scalar mismatch: %+v", got)
	}
	if got.Description != want.Description || got.Avatar != want.Avatar || got.Project != want.Project {
		t.Errorf("scalar mismatch (2): %+v", got)
	}
	if got.Status != want.Status {
		t.Errorf("status = %q want %q", got.Status, want.Status)
	}
	if got.HealthStatus != want.HealthStatus || got.HostAddress != want.HostAddress || got.LastUpdatedBy != want.LastUpdatedBy {
		t.Errorf("cross-kind columns mismatch: %+v", got)
	}
	if got.Callback == nil || got.Callback.Scheme != want.Callback.Scheme || got.Callback.Target != want.Callback.Target {
		t.Errorf("callback mismatch: got=%v want=%v", got.Callback, want.Callback)
	}
	if string(got.KindMeta) != string(want.KindMeta) {
		t.Errorf("kind_meta: got=%s want=%s", got.KindMeta, want.KindMeta)
	}
	if got.CachedAt == nil || !got.CachedAt.Equal(*want.CachedAt) {
		t.Errorf("cached_at mismatch: %v vs %v", got.CachedAt, want.CachedAt)
	}
	if got.LastSeenAt == nil || !got.LastSeenAt.Equal(*want.LastSeenAt) {
		t.Errorf("last_seen_at mismatch: %v vs %v", got.LastSeenAt, want.LastSeenAt)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("timestamps mismatch: created=%v updated=%v want created=%v updated=%v",
			got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}

	if !stringSetEqual(got.Capabilities, want.Capabilities) {
		t.Errorf("capabilities = %v want %v", got.Capabilities, want.Capabilities)
	}
	if len(got.Skills) != len(want.Skills) {
		t.Fatalf("skills count = %d want %d", len(got.Skills), len(want.Skills))
	}
	for _, w := range want.Skills {
		found := false
		for _, g := range got.Skills {
			if g.Name == w.Name {
				if g.Via != w.Via || g.Level != w.Level || !g.LearnedAt.Equal(w.LearnedAt) {
					t.Errorf("skill %q mismatch: got %+v want %+v", w.Name, g, w)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("skill %q missing from result", w.Name)
		}
	}
	if len(got.Links) != len(want.Links) {
		t.Fatalf("links count = %d want %d", len(got.Links), len(want.Links))
	}
}

func TestStorage_InsertProfile_DefaultsApplied(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_default5678"

	p := registry.Profile{
		URN:         urn,
		Kind:        registry.KindAgent,
		DisplayName: "Defaults",
		// MuxInstanceID empty → "agent-mux"
		// Status empty → StatusActive
		// CreatedAt/UpdatedAt zero → time.Now().UTC()
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := st.InsertProfile(ctx, p); err != nil {
		t.Fatalf("InsertProfile: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	got, err := st.GetProfile(ctx, urn)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if got.MuxInstanceID != "agent-mux" {
		t.Errorf("mux_instance_id default = %q want %q", got.MuxInstanceID, "agent-mux")
	}
	if got.Status != registry.StatusActive {
		t.Errorf("status default = %q want %q", got.Status, registry.StatusActive)
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Errorf("created_at default = %v not in [%v, %v]", got.CreatedAt, before, after)
	}
	if got.UpdatedAt.Before(before) || got.UpdatedAt.After(after) {
		t.Errorf("updated_at default = %v not in [%v, %v]", got.UpdatedAt, before, after)
	}
}

func TestStorage_GetProfile_NotFound(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	_, err := st.GetProfile(ctx, "msg://agent/agent-mux/agt_nopenope12")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStorage_URNExists(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_exists0001"
	insertMinimal(t, st, urn)

	ok, err := st.URNExists(ctx, urn)
	if err != nil {
		t.Fatalf("URNExists: %v", err)
	}
	if !ok {
		t.Errorf("URNExists(present) = false; want true")
	}
	ok, err = st.URNExists(ctx, "msg://agent/agent-mux/agt_absent0001")
	if err != nil {
		t.Fatalf("URNExists: %v", err)
	}
	if ok {
		t.Errorf("URNExists(absent) = true; want false")
	}
}

func TestStorage_UpdateProfileFields_ScalarAndBumpsUpdatedAt(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_update0001"

	p := sampleProfile(t, urn)
	if err := st.InsertProfile(ctx, p); err != nil {
		t.Fatalf("InsertProfile: %v", err)
	}
	originalUpdated := p.UpdatedAt

	// Small sleep so the bumped updated_at is distinguishable from the
	// inserted value at second-resolution at least.
	time.Sleep(2 * time.Millisecond)

	if err := st.UpdateProfileFields(ctx, urn, map[string]any{
		"title":       "Sprint Lead",
		"role":        "lead",
		"description": "Updated description",
	}); err != nil {
		t.Fatalf("UpdateProfileFields: %v", err)
	}

	got, err := st.GetProfile(ctx, urn)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if got.Title != "Sprint Lead" || got.Role != "lead" || got.Description != "Updated description" {
		t.Errorf("update did not apply: %+v", got)
	}
	if !got.UpdatedAt.After(originalUpdated) {
		t.Errorf("updated_at not bumped: original=%v new=%v", originalUpdated, got.UpdatedAt)
	}
}

func TestStorage_UpdateProfileFields_RejectsUnknownColumn(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_unknown001"
	insertMinimal(t, st, urn)

	// Free-text bogus column.
	err := st.UpdateProfileFields(ctx, urn, map[string]any{"not_a_column": "x"})
	if !errors.Is(err, registry.ErrUnknownColumn) {
		t.Errorf("bogus column err = %v, want ErrUnknownColumn", err)
	}

	// Immutable columns are also rejected.
	for _, col := range []string{"urn", "kind", "mux_instance_id", "created_at", "updated_at"} {
		err := st.UpdateProfileFields(ctx, urn, map[string]any{col: "x"})
		if !errors.Is(err, registry.ErrUnknownColumn) {
			t.Errorf("immutable col %q err = %v, want ErrUnknownColumn", col, err)
		}
	}
}

func TestStorage_UpdateProfileFields_NotFound(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	err := st.UpdateProfileFields(ctx, "msg://agent/agent-mux/agt_missing00", map[string]any{"title": "x"})
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestStorage_ReplaceCapabilities_Atomic(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_replace001"

	p := sampleProfile(t, urn)
	if err := st.InsertProfile(ctx, p); err != nil {
		t.Fatalf("InsertProfile: %v", err)
	}

	if err := st.ReplaceCapabilities(ctx, urn, []string{"rust", "elixir"}); err != nil {
		t.Fatalf("ReplaceCapabilities: %v", err)
	}
	got, err := st.GetProfile(ctx, urn)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !stringSetEqual(got.Capabilities, []string{"rust", "elixir"}) {
		t.Errorf("after replace caps = %v want [rust elixir]", got.Capabilities)
	}

	// Replace with empty slice — clears.
	if err := st.ReplaceCapabilities(ctx, urn, nil); err != nil {
		t.Fatalf("ReplaceCapabilities (empty): %v", err)
	}
	got, _ = st.GetProfile(ctx, urn)
	if len(got.Capabilities) != 0 {
		t.Errorf("after empty replace caps = %v want empty", got.Capabilities)
	}
}

func TestStorage_ReplaceSkills_TransactionAtomicity(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_txatomic01"

	p := sampleProfile(t, urn)
	if err := st.InsertProfile(ctx, p); err != nil {
		t.Fatalf("InsertProfile: %v", err)
	}
	preReplace, _ := st.GetProfile(ctx, urn)

	// Two skills with the same Name violate the (urn, name) PK on the second
	// INSERT. The transaction must roll back, leaving the pre-replace state.
	learned := fixedTime(t, "2026-05-16T09:00:00Z")
	bad := []registry.Skill{
		{Name: "duplicate", LearnedAt: learned},
		{Name: "duplicate", LearnedAt: learned},
	}
	if err := st.ReplaceSkills(ctx, urn, bad); err == nil {
		t.Fatal("ReplaceSkills with duplicate name succeeded; want PK violation")
	}

	got, err := st.GetProfile(ctx, urn)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if len(got.Skills) != len(preReplace.Skills) {
		t.Errorf("skills count after failed tx = %d, want unchanged %d", len(got.Skills), len(preReplace.Skills))
	}
	// Original skill names must still be present.
	for _, w := range preReplace.Skills {
		found := false
		for _, g := range got.Skills {
			if g.Name == w.Name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("skill %q lost after failed tx", w.Name)
		}
	}
}

func TestStorage_ReplaceLinks(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_links00001"

	p := sampleProfile(t, urn)
	if err := st.InsertProfile(ctx, p); err != nil {
		t.Fatalf("InsertProfile: %v", err)
	}

	newLinks := []registry.Link{
		{Kind: "team_lead", Target: "msg://agent/agent-mux/agt_lead000001"},
	}
	if err := st.ReplaceLinks(ctx, urn, newLinks); err != nil {
		t.Fatalf("ReplaceLinks: %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if len(got.Links) != 1 || got.Links[0].Kind != "team_lead" {
		t.Errorf("links after replace = %v, want one team_lead", got.Links)
	}
}

func TestStorage_AppendCapabilities_IgnoresDuplicates(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_append0001"
	insertMinimal(t, st, urn)

	if err := st.AppendCapabilities(ctx, urn, []string{"a", "b"}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := st.AppendCapabilities(ctx, urn, []string{"b", "c"}); err != nil {
		t.Fatalf("append 2 (dup b): %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if !stringSetEqual(got.Capabilities, []string{"a", "b", "c"}) {
		t.Errorf("caps after append-with-dup = %v want [a b c]", got.Capabilities)
	}
}

func TestStorage_AppendSkills_DoesNotRefreshLearnedAt(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_appendsk01"
	insertMinimal(t, st, urn)

	t1 := fixedTime(t, "2026-05-10T12:00:00Z")
	t2 := fixedTime(t, "2026-05-15T12:00:00Z")
	if err := st.AppendSkills(ctx, urn, []registry.Skill{{Name: "go", LearnedAt: t1, Level: "intermediate"}}); err != nil {
		t.Fatalf("AppendSkills 1: %v", err)
	}
	// Re-append same Name with a later LearnedAt — INSERT OR IGNORE must
	// NOT overwrite the existing row.
	if err := st.AppendSkills(ctx, urn, []registry.Skill{{Name: "go", LearnedAt: t2, Level: "expert"}}); err != nil {
		t.Fatalf("AppendSkills 2: %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if len(got.Skills) != 1 {
		t.Fatalf("skills = %v, want 1", got.Skills)
	}
	if !got.Skills[0].LearnedAt.Equal(t1) {
		t.Errorf("LearnedAt = %v, want unchanged %v", got.Skills[0].LearnedAt, t1)
	}
	if got.Skills[0].Level != "intermediate" {
		t.Errorf("Level = %q, want unchanged intermediate", got.Skills[0].Level)
	}
}

func TestStorage_AppendLinks(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_appendln01"
	insertMinimal(t, st, urn)

	links := []registry.Link{
		{Kind: "repo", Target: "https://r1.invalid"},
		{Kind: "repo", Target: "https://r1.invalid"}, // dup
		{Kind: "team_lead", Target: "msg://agent/agent-mux/agt_lead123456"},
	}
	if err := st.AppendLinks(ctx, urn, links); err != nil {
		t.Fatalf("AppendLinks: %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if len(got.Links) != 2 {
		t.Errorf("links = %v, want 2 (dup ignored)", got.Links)
	}
}

func TestStorage_RemoveCapabilities(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_remove0001"
	insertMinimal(t, st, urn)

	if err := st.AppendCapabilities(ctx, urn, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := st.RemoveCapabilities(ctx, urn, []string{"b"}); err != nil {
		t.Fatalf("RemoveCapabilities: %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if !stringSetEqual(got.Capabilities, []string{"a", "c"}) {
		t.Errorf("after remove = %v want [a c]", got.Capabilities)
	}
}

func TestStorage_RemoveSkills(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_remskill01"
	insertMinimal(t, st, urn)
	learned := fixedTime(t, "2026-05-10T12:00:00Z")
	if err := st.AppendSkills(ctx, urn, []registry.Skill{
		{Name: "go", LearnedAt: learned},
		{Name: "rust", LearnedAt: learned},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.RemoveSkills(ctx, urn, []string{"go"}); err != nil {
		t.Fatalf("RemoveSkills: %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if len(got.Skills) != 1 || got.Skills[0].Name != "rust" {
		t.Errorf("skills after remove = %v want [rust]", got.Skills)
	}
}

func TestStorage_RemoveLinks(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_remlinks01"
	insertMinimal(t, st, urn)
	if err := st.AppendLinks(ctx, urn, []registry.Link{
		{Kind: "repo", Target: "https://r1.invalid"},
		{Kind: "repo", Target: "https://r2.invalid"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.RemoveLinks(ctx, urn, []registry.Link{
		{Kind: "repo", Target: "https://r1.invalid"},
	}); err != nil {
		t.Fatalf("RemoveLinks: %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if len(got.Links) != 1 || got.Links[0].Target != "https://r2.invalid" {
		t.Errorf("links after remove = %v want [r2]", got.Links)
	}
}

func TestStorage_RemoveEmpty_NoOp(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_removenoop"
	insertMinimal(t, st, urn)
	if err := st.AppendCapabilities(ctx, urn, []string{"a"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, _ := st.GetProfile(ctx, urn)
	beforeUpdated := before.UpdatedAt

	time.Sleep(2 * time.Millisecond)
	if err := st.RemoveCapabilities(ctx, urn, nil); err != nil {
		t.Errorf("RemoveCapabilities(nil): %v", err)
	}
	if err := st.RemoveSkills(ctx, urn, nil); err != nil {
		t.Errorf("RemoveSkills(nil): %v", err)
	}
	if err := st.RemoveLinks(ctx, urn, nil); err != nil {
		t.Errorf("RemoveLinks(nil): %v", err)
	}
	after, _ := st.GetProfile(ctx, urn)
	if !after.UpdatedAt.Equal(beforeUpdated) {
		t.Errorf("updated_at changed on empty-input remove: %v → %v", beforeUpdated, after.UpdatedAt)
	}
}

func TestStorage_SoftDelete_PreservesChildTables(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_softdel001"

	p := sampleProfile(t, urn)
	if err := st.InsertProfile(ctx, p); err != nil {
		t.Fatalf("InsertProfile: %v", err)
	}
	before, _ := st.GetProfile(ctx, urn)
	beforeUpdated := before.UpdatedAt

	time.Sleep(2 * time.Millisecond)
	if err := st.SoftDelete(ctx, urn); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	got, err := st.GetProfile(ctx, urn)
	if err != nil {
		t.Fatalf("GetProfile after soft-delete: %v", err)
	}
	if got.Status != registry.StatusDeprecated {
		t.Errorf("status after soft delete = %q want deprecated", got.Status)
	}
	if !got.UpdatedAt.After(beforeUpdated) {
		t.Errorf("updated_at not bumped on soft delete: before=%v after=%v", beforeUpdated, got.UpdatedAt)
	}

	// Load-bearing assertion: child tables UNTOUCHED (D11 amended T-02 criterion).
	if !stringSetEqual(got.Capabilities, before.Capabilities) {
		t.Errorf("capabilities changed after soft delete: before=%v after=%v",
			before.Capabilities, got.Capabilities)
	}
	if len(got.Skills) != len(before.Skills) {
		t.Errorf("skills count changed after soft delete: before=%d after=%d",
			len(before.Skills), len(got.Skills))
	}
	if len(got.Links) != len(before.Links) {
		t.Errorf("links count changed after soft delete: before=%d after=%d",
			len(before.Links), len(got.Links))
	}
}

func TestStorage_SoftDelete_NotFound(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	err := st.SoftDelete(ctx, "msg://agent/agent-mux/agt_softmiss01")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("SoftDelete(missing) = %v, want ErrNotFound", err)
	}
}

func TestStorage_BumpCachedAt(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	const urn = "msg://agent/agent-mux/agt_cached0001"
	insertMinimal(t, st, urn)
	before, _ := st.GetProfile(ctx, urn)
	beforeUpdated := before.UpdatedAt

	target := fixedTime(t, "2026-05-21T08:00:00Z")
	time.Sleep(2 * time.Millisecond)
	if err := st.BumpCachedAt(ctx, urn, target); err != nil {
		t.Fatalf("BumpCachedAt: %v", err)
	}
	got, _ := st.GetProfile(ctx, urn)
	if got.CachedAt == nil || !got.CachedAt.Equal(target) {
		t.Errorf("cached_at = %v, want %v", got.CachedAt, target)
	}
	if !got.UpdatedAt.After(beforeUpdated) {
		t.Errorf("updated_at not bumped: before=%v after=%v", beforeUpdated, got.UpdatedAt)
	}
}

func TestStorage_BumpCachedAt_NotFound(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	err := st.BumpCachedAt(ctx, "msg://agent/agent-mux/agt_bumpmiss01", time.Now().UTC())
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("BumpCachedAt(missing) = %v, want ErrNotFound", err)
	}
}

// ─── Search coverage ───────────────────────────────────────────────────────────

// seedSearchCorpus inserts a curated set of profiles to exercise filters.
// Returns the URNs in alphabetical-by-display-name order so callers can
// assert ordering directly.
func seedSearchCorpus(t *testing.T, st *registry.Storage) []string {
	t.Helper()
	ctx := context.Background()
	learned := fixedTime(t, "2026-05-01T00:00:00Z")
	created := fixedTime(t, "2026-05-01T00:00:00Z")

	rows := []registry.Profile{
		{
			URN: "msg://agent/agent-mux/agt_aaa0000001", Kind: registry.KindAgent,
			DisplayName: "Alpha", Role: "implementer", Title: "Engineer", Project: "tether",
			Status: registry.StatusActive, CreatedAt: created, UpdatedAt: created,
			Capabilities: []string{"go", "sqlite"},
			Skills:       []registry.Skill{{Name: "registry-design", LearnedAt: learned}},
		},
		{
			URN: "msg://agent/agent-mux/agt_bbb0000002", Kind: registry.KindAgent,
			DisplayName: "Bravo", Role: "lead", Title: "Engineer", Project: "tether",
			Status: registry.StatusActive, CreatedAt: created, UpdatedAt: created,
			Capabilities: []string{"go"},
			Skills:       []registry.Skill{{Name: "callback-routing", LearnedAt: learned}},
		},
		{
			URN: "msg://agent/agent-mux/agt_ccc0000003", Kind: registry.KindAgent,
			DisplayName: "Charlie", Role: "implementer", Title: "Architect", Project: "cerberus",
			Status: registry.StatusActive, CreatedAt: created, UpdatedAt: created,
			Capabilities: []string{"yaml"},
		},
		{
			URN: "msg://agent/agent-mux/agt_ddd0000004", Kind: registry.KindAgent,
			DisplayName: "Delta", Role: "implementer", Title: "Engineer", Project: "tether",
			Status: registry.StatusDeprecated, CreatedAt: created, UpdatedAt: created,
		},
		// Different kind — should be excluded from agent-kind searches.
		{
			URN: "msg://agent/agent-mux/prj_zzz0000099", Kind: registry.KindProject,
			DisplayName: "ZetaProject", Status: registry.StatusActive, CreatedAt: created, UpdatedAt: created,
		},
	}
	for _, p := range rows {
		if err := st.InsertProfile(ctx, p); err != nil {
			t.Fatalf("seed insert %q: %v", p.URN, err)
		}
	}
	return []string{
		"msg://agent/agent-mux/agt_aaa0000001",
		"msg://agent/agent-mux/agt_bbb0000002",
		"msg://agent/agent-mux/agt_ccc0000003",
	}
}

func TestStorage_Search_DefaultExcludesDeprecated(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	wantOrder := seedSearchCorpus(t, st)

	got, err := st.Search(ctx, registry.KindAgent, registry.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("default search count = %d want 3 (deprecated Delta excluded)", len(got))
	}
	for i, p := range got {
		if p.URN != wantOrder[i] {
			t.Errorf("position %d = %q want %q", i, p.URN, wantOrder[i])
		}
	}
}

func TestStorage_Search_FilterByRole(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	seedSearchCorpus(t, st)

	got, err := st.Search(ctx, registry.KindAgent, registry.Filter{Role: "implementer"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("role-implementer count = %d want 2 (Alpha + Charlie)", len(got))
	}
	if got[0].DisplayName != "Alpha" || got[1].DisplayName != "Charlie" {
		t.Errorf("order = [%s, %s] want [Alpha, Charlie]", got[0].DisplayName, got[1].DisplayName)
	}
}

func TestStorage_Search_FilterByTitle(t *testing.T) {
	st := newStorage(t)
	ctx := context.Background()
	seedSearchCorpus(t, st)
	got, _ := st.Search(context.Background(), registry.KindAgent, registry.Filter{Title: "Architect"})
	_ = ctx
	if len(got) != 1 || got[0].DisplayName != "Charlie" {
		t.Errorf("title=Architect = %v, want [Charlie]", displayNames(got))
	}
}

func TestStorage_Search_FilterByProject(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, _ := st.Search(context.Background(), registry.KindAgent, registry.Filter{Project: "tether"})
	if len(got) != 2 {
		t.Fatalf("project=tether count = %d want 2", len(got))
	}
}

func TestStorage_Search_FilterByCapability(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, err := st.Search(context.Background(), registry.KindAgent, registry.Filter{Capability: "sqlite"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].DisplayName != "Alpha" {
		t.Errorf("capability=sqlite = %v want [Alpha]", displayNames(got))
	}
}

func TestStorage_Search_FilterBySkill(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, err := st.Search(context.Background(), registry.KindAgent, registry.Filter{SkillName: "callback-routing"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].DisplayName != "Bravo" {
		t.Errorf("skill=callback-routing = %v want [Bravo]", displayNames(got))
	}
}

func TestStorage_Search_CombinedFilters(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, _ := st.Search(context.Background(), registry.KindAgent, registry.Filter{
		Role:       "implementer",
		Project:    "tether",
		Capability: "go",
	})
	if len(got) != 1 || got[0].DisplayName != "Alpha" {
		t.Errorf("combined = %v want [Alpha]", displayNames(got))
	}
}

func TestStorage_Search_StatusDeprecated(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, _ := st.Search(context.Background(), registry.KindAgent, registry.Filter{Status: string(registry.StatusDeprecated)})
	if len(got) != 1 || got[0].DisplayName != "Delta" {
		t.Errorf("status=deprecated = %v want [Delta]", displayNames(got))
	}
}

func TestStorage_Search_StatusAny(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, _ := st.Search(context.Background(), registry.KindAgent, registry.Filter{Status: registry.StatusAny})
	if len(got) != 4 {
		t.Errorf("status=* count = %d want 4 (all agent kinds incl deprecated)", len(got))
	}
}

func TestStorage_Search_KindIsolation(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, _ := st.Search(context.Background(), registry.KindProject, registry.Filter{})
	if len(got) != 1 || got[0].DisplayName != "ZetaProject" {
		t.Errorf("kind=project = %v want [ZetaProject]", displayNames(got))
	}
}

func TestStorage_Search_EmptyResult(t *testing.T) {
	st := newStorage(t)
	seedSearchCorpus(t, st)
	got, err := st.Search(context.Background(), registry.KindAgent, registry.Filter{Role: "no-such-role"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty filter result = %v want []", displayNames(got))
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────────

// insertMinimal inserts a stripped-down agent profile for tests that don't
// care about every field. created_at/updated_at default to now.
func insertMinimal(t *testing.T, st *registry.Storage, urn string) {
	t.Helper()
	p := registry.Profile{
		URN:         urn,
		Kind:        registry.KindAgent,
		DisplayName: "Minimal " + urn,
	}
	if err := st.InsertProfile(context.Background(), p); err != nil {
		t.Fatalf("insertMinimal %q: %v", urn, err)
	}
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
		if m[v] < 0 {
			return false
		}
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func displayNames(ps []registry.Profile) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.DisplayName
	}
	return out
}
