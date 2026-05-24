package registry_test

// service_test.go — coverage for the registry service core (T-v060-01-03).
//
// Matrix:
//   - Register happy path (agent + project): correct prefix, URN format,
//     canonical defaults from the storage layer reload.
//   - Register validation: caller-supplied URN, bad kind, empty
//     display_name, malformed skill (missing name / zero learned_at).
//   - Register URN collision-retry: stub storageBackend that collides the
//     first N-1 times then succeeds; ErrMintExhausted at the boundary.
//   - Lookup happy + ErrNotFound passthrough.
//   - UpdateSelf scalar partial-merge: only listed fields change,
//     updated_at bumps, last_updated_by recorded.
//   - UpdateSelf array merge — table-driven across {capabilities, skills,
//     links} × {shorthand REPLACE, explicit REPLACE, APPEND, REMOVE} ×
//     {non-empty, empty-no-op}.
//   - UpdateSelf missing last_updated_by → ErrInvalidRequest.
//   - UpdateSelf on unknown URN → ErrNotFound.
//   - UpdateSelf concurrent: two goroutines, different scalars, both
//     applied (last-writer-wins on collisions).
//   - Deregister happy: status flips to deprecated, child tables intact.
//   - Deregister on unknown URN → ErrNotFound.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// newService wraps newStorage so service-level tests get a real Service
// bound to a real in-memory SQLite. Reuses newStorage from storage_test.go.
func newService(t *testing.T) *registry.Service {
	t.Helper()
	return registry.NewService(newStorage(t))
}

// newServiceForConcurrency builds a Service against an in-memory SQLite
// pinned to MaxOpenConns=1 — the production invariant from
// internal/store/sqlite.go. Without it, two goroutines could each grab a
// separate ":memory:" connection (each with its own empty DB) and the
// concurrency test fails with "no such table" rather than exercising the
// serialization property under test.
func newServiceForConcurrency(t *testing.T) *registry.Service {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return registry.NewService(registry.NewStorage(db))
}

// ─── Register ────────────────────────────────────────────────────────────────

func TestService_Register_AgentHappyPath(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	in := registry.Profile{
		DisplayName:  "Alpha",
		Role:         "implementer",
		Title:        "Engineer",
		Project:      "tether",
		Capabilities: []string{"go"},
	}
	got, err := svc.Register(ctx, registry.KindAgent, in)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got.URN == "" {
		t.Fatal("URN empty after Register")
	}
	const prefix = "msg://agent/agent-mux/agt_"
	if len(got.URN) != len(prefix)+10 {
		t.Errorf("URN length = %d, want %d", len(got.URN), len(prefix)+10)
	}
	if got.URN[:len(prefix)] != prefix {
		t.Errorf("URN = %q, want prefix %q", got.URN, prefix)
	}
	if got.Kind != registry.KindAgent {
		t.Errorf("Kind = %q, want %q", got.Kind, registry.KindAgent)
	}
	if got.Status != registry.StatusActive {
		t.Errorf("Status = %q, want %q (default)", got.Status, registry.StatusActive)
	}
	if got.MuxInstanceID != "agent-mux" {
		t.Errorf("MuxInstanceID = %q, want %q", got.MuxInstanceID, "agent-mux")
	}
	if got.LastUpdatedBy != "system:register" {
		t.Errorf("LastUpdatedBy = %q, want %q (placeholder)", got.LastUpdatedBy, "system:register")
	}
	if !stringSetEqual(got.Capabilities, []string{"go"}) {
		t.Errorf("Capabilities = %v, want [go]", got.Capabilities)
	}
}

func TestService_Register_ProjectHappyPath(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	got, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Tether",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	const prefix = "msg://agent/agent-mux/prj_"
	if got.URN[:len(prefix)] != prefix {
		t.Errorf("URN = %q, want prefix %q", got.URN, prefix)
	}
	if got.Kind != registry.KindProject {
		t.Errorf("Kind = %q, want %q", got.Kind, registry.KindProject)
	}
}

func TestService_Register_HonorsLastUpdatedBy(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	got, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName:   "Bravo",
		LastUpdatedBy: "bootstrap:v060-01",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got.LastUpdatedBy != "bootstrap:v060-01" {
		t.Errorf("LastUpdatedBy = %q, want caller-supplied value", got.LastUpdatedBy)
	}
}

func TestService_Register_Validation(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	learned := fixedTime(t, "2026-05-01T00:00:00Z")

	cases := []struct {
		name string
		kind registry.Kind
		in   registry.Profile
	}{
		{
			name: "caller-supplied URN rejected",
			kind: registry.KindAgent,
			in: registry.Profile{
				URN:         "msg://agent/agent-mux/agt_callersupp1",
				DisplayName: "Bad",
			},
		},
		{
			name: "bad kind rejected",
			kind: registry.Kind("unicorn"),
			in:   registry.Profile{DisplayName: "Bad"},
		},
		{
			name: "empty display_name rejected",
			kind: registry.KindAgent,
			in:   registry.Profile{},
		},
		{
			name: "skill missing name rejected",
			kind: registry.KindAgent,
			in: registry.Profile{
				DisplayName: "Bad",
				Skills: []registry.Skill{
					{Name: "", LearnedAt: learned},
				},
			},
		},
		{
			name: "skill zero LearnedAt rejected",
			kind: registry.KindAgent,
			in: registry.Profile{
				DisplayName: "Bad",
				Skills: []registry.Skill{
					{Name: "go", LearnedAt: time.Time{}},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Register(ctx, tc.kind, tc.in)
			if !errors.Is(err, registry.ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// ─── Register URN collision-retry ────────────────────────────────────────────

// stubStorage is a minimal storageBackend used by collision-retry tests.
// Only InsertProfile / GetProfile / URNExists are exercised; every other
// method panics so an inadvertent dependency surfaces loudly.
//
// urnExistsScript is the sequence of (bool, error) tuples returned by
// successive URNExists calls; calls past the script length return
// (false, nil) so a successful mint can complete.
type stubStorage struct {
	urnExistsScript []bool
	urnExistsCalls  int
	inserted        registry.Profile
	insertErr       error
}

func (s *stubStorage) InsertProfile(_ context.Context, p registry.Profile) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = p
	return nil
}

func (s *stubStorage) GetProfile(_ context.Context, urn string) (registry.Profile, error) {
	if s.inserted.URN == urn {
		return s.inserted, nil
	}
	return registry.Profile{}, registry.ErrNotFound
}

func (s *stubStorage) URNExists(_ context.Context, _ string) (bool, error) {
	defer func() { s.urnExistsCalls++ }()
	if s.urnExistsCalls < len(s.urnExistsScript) {
		return s.urnExistsScript[s.urnExistsCalls], nil
	}
	return false, nil
}

func (s *stubStorage) FindByCallbackTarget(context.Context, string) (registry.Profile, error) {
	panic("stubStorage.FindByCallbackTarget: unexpected call")
}

// All other methods of storageBackend are panic-on-call; the URN-retry
// path doesn't touch them.
func (s *stubStorage) UpdateProfileFields(context.Context, string, map[string]any) error {
	panic("stubStorage.UpdateProfileFields: unexpected call")
}
func (s *stubStorage) ReplaceCapabilities(context.Context, string, []string) error {
	panic("stubStorage.ReplaceCapabilities: unexpected call")
}
func (s *stubStorage) ReplaceSkills(context.Context, string, []registry.Skill) error {
	panic("stubStorage.ReplaceSkills: unexpected call")
}
func (s *stubStorage) ReplaceLinks(context.Context, string, []registry.Link) error {
	panic("stubStorage.ReplaceLinks: unexpected call")
}
func (s *stubStorage) AppendCapabilities(context.Context, string, []string) error {
	panic("stubStorage.AppendCapabilities: unexpected call")
}
func (s *stubStorage) AppendSkills(context.Context, string, []registry.Skill) error {
	panic("stubStorage.AppendSkills: unexpected call")
}
func (s *stubStorage) AppendLinks(context.Context, string, []registry.Link) error {
	panic("stubStorage.AppendLinks: unexpected call")
}
func (s *stubStorage) RemoveCapabilities(context.Context, string, []string) error {
	panic("stubStorage.RemoveCapabilities: unexpected call")
}
func (s *stubStorage) RemoveSkills(context.Context, string, []string) error {
	panic("stubStorage.RemoveSkills: unexpected call")
}
func (s *stubStorage) RemoveLinks(context.Context, string, []registry.Link) error {
	panic("stubStorage.RemoveLinks: unexpected call")
}
func (s *stubStorage) SoftDelete(context.Context, string) error {
	panic("stubStorage.SoftDelete: unexpected call")
}
func (s *stubStorage) BumpCachedAt(context.Context, string, time.Time) error {
	panic("stubStorage.BumpCachedAt: unexpected call")
}
func (s *stubStorage) Search(context.Context, registry.Kind, registry.Filter) ([]registry.Profile, error) {
	panic("stubStorage.Search: unexpected call")
}
func (s *stubStorage) InsertGroupWithOwner(context.Context, registry.Profile, string) error {
	panic("stubStorage.InsertGroupWithOwner: unexpected call")
}
func (s *stubStorage) InsertGroupMember(context.Context, string, string, registry.MemberRole, time.Time) error {
	panic("stubStorage.InsertGroupMember: unexpected call")
}
func (s *stubStorage) GroupMemberRole(context.Context, string, string) (registry.MemberRole, bool, error) {
	panic("stubStorage.GroupMemberRole: unexpected call")
}
func (s *stubStorage) ListGroupsForMember(context.Context, string) ([]registry.Profile, error) {
	panic("stubStorage.ListGroupsForMember: unexpected call")
}
func (s *stubStorage) SetProfileStatus(context.Context, string, registry.Status) error {
	panic("stubStorage.SetProfileStatus: unexpected call")
}
func (s *stubStorage) ListMembers(context.Context, string) ([]registry.GroupMember, error) {
	panic("stubStorage.ListMembers: unexpected call")
}
func (s *stubStorage) RemoveGroupMember(context.Context, string, string) error {
	panic("stubStorage.RemoveGroupMember: unexpected call")
}
func (s *stubStorage) UpdateGroupMemberRole(context.Context, string, string, registry.MemberRole) error {
	panic("stubStorage.UpdateGroupMemberRole: unexpected call")
}
func (s *stubStorage) CountModeratorsExcluding(context.Context, string, string) (int, error) {
	panic("stubStorage.CountModeratorsExcluding: unexpected call")
}
func (s *stubStorage) InsertGroupMessage(context.Context, string, string, string, string, string, json.RawMessage) (registry.GroupMessage, error) {
	panic("stubStorage.InsertGroupMessage: unexpected call")
}
func (s *stubStorage) ListGroupMessages(context.Context, string, int64, string, int, time.Time) ([]registry.GroupMessage, error) {
	panic("stubStorage.ListGroupMessages: unexpected call")
}
func (s *stubStorage) BumpGroupReadCursor(context.Context, string, string, int64) error {
	panic("stubStorage.BumpGroupReadCursor: unexpected call")
}
func (s *stubStorage) GroupMemberJoinedAt(context.Context, string, string) (time.Time, bool, error) {
	panic("stubStorage.GroupMemberJoinedAt: unexpected call")
}
func (s *stubStorage) GroupMemberLastReadSeq(context.Context, string, string) (int64, bool, error) {
	panic("stubStorage.GroupMemberLastReadSeq: unexpected call")
}
func (s *stubStorage) ListMentionsForMember(context.Context, string, time.Time, int) ([]registry.GroupMessage, error) {
	panic("stubStorage.ListMentionsForMember: unexpected call")
}
func (s *stubStorage) FindByDisplayName(context.Context, string) ([]registry.Profile, error) {
	panic("stubStorage.FindByDisplayName: unexpected call")
}
func (s *stubStorage) LookupExternalIDsForURN(context.Context, string) ([]registry.ExternalID, error) {
	panic("stubStorage.LookupExternalIDsForURN: unexpected call")
}
func (s *stubStorage) LookupURNByExternalID(context.Context, registry.Kind, string, string) (string, bool, error) {
	panic("stubStorage.LookupURNByExternalID: unexpected call")
}
func (s *stubStorage) AttachExternalID(context.Context, string, string, string) error {
	panic("stubStorage.AttachExternalID: unexpected call")
}
func (s *stubStorage) DetachExternalID(context.Context, string, string) error {
	panic("stubStorage.DetachExternalID: unexpected call")
}

func TestService_Register_URNCollisionRetry(t *testing.T) {
	// Three collisions then success on the fourth attempt. The minter's
	// internal retry budget (mintMaxRetries = 5) is wider than this, so
	// Register must surface the eventually-minted URN.
	stub := &stubStorage{
		urnExistsScript: []bool{true, true, true, false},
	}
	svc := registry.NewServiceFromBackendForTest(stub)
	ctx := context.Background()

	got, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Retry Me",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if stub.urnExistsCalls != 4 {
		t.Errorf("URNExists calls = %d, want 4 (3 collisions + 1 success)", stub.urnExistsCalls)
	}
	if got.URN == "" {
		t.Fatal("URN empty after retry-then-success")
	}
}

func TestService_Register_URNExhaustionPropagates(t *testing.T) {
	// Every attempt collides — mintURN should give up after mintMaxRetries
	// and return ErrMintExhausted, which Service.Register propagates.
	always := make([]bool, 16) // > mintMaxRetries
	for i := range always {
		always[i] = true
	}
	stub := &stubStorage{urnExistsScript: always}
	svc := registry.NewServiceFromBackendForTest(stub)
	ctx := context.Background()

	_, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Doomed",
	})
	if !errors.Is(err, registry.ErrMintExhausted) {
		t.Fatalf("err = %v, want ErrMintExhausted", err)
	}
}

// ─── Lookup ──────────────────────────────────────────────────────────────────

func TestService_Lookup_HappyAndNotFound(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "L"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := svc.Lookup(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.URN != reg.URN {
		t.Errorf("URN = %q, want %q", got.URN, reg.URN)
	}

	if _, err := svc.Lookup(ctx, "msg://agent/agent-mux/agt_absent00001"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("absent err = %v, want ErrNotFound", err)
	}
}

func TestService_Merge_MovesExternalIDsAndMarksSource(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	src, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName:   "Clockwork",
		Capabilities:  []string{"go"},
		LastUpdatedBy: "tester",
		KindMeta:      json.RawMessage(`{"src":{"owner":"clockwork"}}`),
	})
	if err != nil {
		t.Fatalf("register src: %v", err)
	}
	dst, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName:   "Clockwork Canonical",
		Capabilities:  []string{"sqlite"},
		LastUpdatedBy: "tester",
		KindMeta:      json.RawMessage(`{"dst":{"owner":"clockwork"}}`),
	})
	if err != nil {
		t.Fatalf("register dst: %v", err)
	}
	if err := svc.AttachExternalID(ctx, src.URN, "cerberus", "clockwork"); err != nil {
		t.Fatalf("attach src extid: %v", err)
	}
	if err := svc.AttachExternalID(ctx, dst.URN, "tether", "clockwork"); err != nil {
		t.Fatalf("attach dst extid: %v", err)
	}

	merged, err := svc.Merge(ctx, src.URN, dst.URN)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !stringSetEqual(merged.Capabilities, []string{"go", "sqlite"}) {
		t.Fatalf("merged capabilities = %v", merged.Capabilities)
	}
	if _, ok := merged.ExternalIDFor("tether"); !ok {
		t.Fatalf("merged dst missing tether external id: %+v", merged.ExternalIDs)
	}
	if ext, ok := merged.ExternalIDFor("cerberus"); !ok || ext.ExternalID != "clockwork" {
		t.Fatalf("merged dst missing cerberus external id: %+v", merged.ExternalIDs)
	}
	var meta map[string]any
	if err := json.Unmarshal(merged.KindMeta, &meta); err != nil {
		t.Fatalf("unmarshal kind_meta: %v", err)
	}
	if _, ok := meta["src"]; !ok {
		t.Fatalf("merged kind_meta missing src payload: %v", meta)
	}
	if _, ok := meta["dst"]; !ok {
		t.Fatalf("merged kind_meta missing dst payload: %v", meta)
	}

	srcAfter, err := svc.Lookup(ctx, src.URN)
	if err != nil {
		t.Fatalf("lookup src after merge: %v", err)
	}
	if srcAfter.Status != registry.StatusMerged {
		t.Fatalf("src status = %q, want %q", srcAfter.Status, registry.StatusMerged)
	}
	if srcAfter.MergedInto != dst.URN {
		t.Fatalf("src merged_into = %q, want %q", srcAfter.MergedInto, dst.URN)
	}
	if len(srcAfter.ExternalIDs) != 0 {
		t.Fatalf("src external_ids after merge = %+v, want empty", srcAfter.ExternalIDs)
	}
}

// ─── UpdateSelf scalar ───────────────────────────────────────────────────────

func TestService_UpdateSelf_ScalarPartialMerge(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Before",
		Title:       "OldTitle",
		Role:        "OldRole",
		Description: "OldDesc",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	originalUpdated := reg.UpdatedAt
	time.Sleep(2 * time.Millisecond)

	newTitle := "NewTitle"
	newDesc := "NewDesc"
	got, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
		Title:         &newTitle,
		Description:   &newDesc,
		LastUpdatedBy: "test:updater",
	})
	if err != nil {
		t.Fatalf("UpdateSelf: %v", err)
	}

	if got.Title != "NewTitle" {
		t.Errorf("Title = %q, want NewTitle", got.Title)
	}
	if got.Description != "NewDesc" {
		t.Errorf("Description = %q, want NewDesc", got.Description)
	}
	if got.Role != "OldRole" {
		t.Errorf("Role = %q, want OldRole (untouched)", got.Role)
	}
	if got.DisplayName != "Before" {
		t.Errorf("DisplayName = %q, want unchanged", got.DisplayName)
	}
	if !got.UpdatedAt.After(originalUpdated) {
		t.Errorf("UpdatedAt not bumped: original=%v new=%v", originalUpdated, got.UpdatedAt)
	}
	if got.LastUpdatedBy != "test:updater" {
		t.Errorf("LastUpdatedBy = %q, want test:updater", got.LastUpdatedBy)
	}
}

func TestService_UpdateSelf_MissingLastUpdatedBy(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "x"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	title := "z"
	_, err = svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{Title: &title})
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestService_UpdateSelf_UnknownURN(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, err := svc.UpdateSelf(ctx, "msg://agent/agent-mux/agt_missing0001", registry.UpdatePatch{
		LastUpdatedBy: "test",
	})
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ─── UpdateSelf array merge (3 fields × 4 modes × empty no-op) ───────────────

func TestService_UpdateSelf_ArrayMerge(t *testing.T) {
	learned := fixedTime(t, "2026-05-01T00:00:00Z")

	type scenario struct {
		mode      registry.ArrayMode
		shorthand bool // skip Mode field; rely on ArrayPatch.Mode literal
	}
	modeScenarios := []scenario{
		{mode: registry.ArrayModeReplace, shorthand: true},
		{mode: registry.ArrayModeReplace, shorthand: false},
		{mode: registry.ArrayModeAppend},
		{mode: registry.ArrayModeRemove},
	}

	for _, ms := range modeScenarios {
		ms := ms
		modeLabel := string(ms.mode)
		if ms.shorthand {
			modeLabel = "shorthand-" + modeLabel
		}
		t.Run("mode="+modeLabel, func(t *testing.T) {
			t.Run("capabilities", func(t *testing.T) {
				runCapabilitiesScenario(t, ms.mode)
			})
			t.Run("skills", func(t *testing.T) {
				runSkillsScenario(t, ms.mode, learned)
			})
			t.Run("links", func(t *testing.T) {
				runLinksScenario(t, ms.mode)
			})
		})
	}
}

func runCapabilitiesScenario(t *testing.T, mode registry.ArrayMode) {
	t.Helper()
	svc := newService(t)
	ctx := context.Background()
	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName:  "Caps",
		Capabilities: []string{"go", "sqlite"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	t.Run("non-empty", func(t *testing.T) {
		var (
			value []string
			want  []string
		)
		switch mode {
		case registry.ArrayModeReplace:
			value = []string{"rust", "elixir"}
			want = []string{"rust", "elixir"}
		case registry.ArrayModeAppend:
			value = []string{"haskell"}
			want = []string{"go", "sqlite", "haskell"}
		case registry.ArrayModeRemove:
			value = []string{"sqlite"}
			want = []string{"go"}
		}
		got, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Capabilities:  &registry.ArrayPatch[string]{Mode: mode, Value: value},
			LastUpdatedBy: "t",
		})
		if err != nil {
			t.Fatalf("UpdateSelf: %v", err)
		}
		if !stringSetEqual(got.Capabilities, want) {
			t.Errorf("capabilities = %v, want %v", got.Capabilities, want)
		}
	})

	t.Run("empty-no-op", func(t *testing.T) {
		before, _ := svc.Lookup(ctx, reg.URN)
		got, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Capabilities:  &registry.ArrayPatch[string]{Mode: mode, Value: nil},
			LastUpdatedBy: "t",
		})
		if err != nil {
			t.Fatalf("UpdateSelf: %v", err)
		}
		if !stringSetEqual(got.Capabilities, before.Capabilities) {
			t.Errorf("empty Value should be no-op: got %v, want %v",
				got.Capabilities, before.Capabilities)
		}
	})
}

func runSkillsScenario(t *testing.T, mode registry.ArrayMode, learned time.Time) {
	t.Helper()
	svc := newService(t)
	ctx := context.Background()
	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Skills",
		Skills: []registry.Skill{
			{Name: "design", LearnedAt: learned},
			{Name: "review", LearnedAt: learned},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	t.Run("non-empty", func(t *testing.T) {
		var (
			value   []registry.Skill
			wantSet map[string]struct{}
		)
		switch mode {
		case registry.ArrayModeReplace:
			value = []registry.Skill{{Name: "writing", LearnedAt: learned}}
			wantSet = map[string]struct{}{"writing": {}}
		case registry.ArrayModeAppend:
			value = []registry.Skill{{Name: "writing", LearnedAt: learned}}
			wantSet = map[string]struct{}{"design": {}, "review": {}, "writing": {}}
		case registry.ArrayModeRemove:
			// Service extracts Name from each Skill; LearnedAt on input
			// doesn't matter for remove.
			value = []registry.Skill{{Name: "review"}}
			wantSet = map[string]struct{}{"design": {}}
		}
		got, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Skills:        &registry.ArrayPatch[registry.Skill]{Mode: mode, Value: value},
			LastUpdatedBy: "t",
		})
		if err != nil {
			t.Fatalf("UpdateSelf: %v", err)
		}
		if len(got.Skills) != len(wantSet) {
			t.Fatalf("skills count = %d, want %d (%v vs %v)",
				len(got.Skills), len(wantSet), skillNames(got.Skills), wantSet)
		}
		for _, s := range got.Skills {
			if _, ok := wantSet[s.Name]; !ok {
				t.Errorf("unexpected skill %q in result", s.Name)
			}
		}
	})

	t.Run("empty-no-op", func(t *testing.T) {
		before, _ := svc.Lookup(ctx, reg.URN)
		got, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Skills:        &registry.ArrayPatch[registry.Skill]{Mode: mode, Value: nil},
			LastUpdatedBy: "t",
		})
		if err != nil {
			t.Fatalf("UpdateSelf: %v", err)
		}
		if len(got.Skills) != len(before.Skills) {
			t.Errorf("empty Value should be no-op: got %v, want %v",
				skillNames(got.Skills), skillNames(before.Skills))
		}
	})
}

func runLinksScenario(t *testing.T, mode registry.ArrayMode) {
	t.Helper()
	svc := newService(t)
	ctx := context.Background()
	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Links",
		Links: []registry.Link{
			{Kind: "repo", Target: "https://r1.invalid"},
			{Kind: "team_lead", Target: "msg://agent/agent-mux/agt_lead000001"},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	t.Run("non-empty", func(t *testing.T) {
		var (
			value    []registry.Link
			wantSet  map[string]struct{}
			wantSize int
		)
		switch mode {
		case registry.ArrayModeReplace:
			value = []registry.Link{{Kind: "repo", Target: "https://r2.invalid"}}
			wantSet = map[string]struct{}{"repo|https://r2.invalid": {}}
			wantSize = 1
		case registry.ArrayModeAppend:
			value = []registry.Link{{Kind: "pipeline", Target: "msg://x"}}
			wantSet = map[string]struct{}{
				"repo|https://r1.invalid":                        {},
				"team_lead|msg://agent/agent-mux/agt_lead000001": {},
				"pipeline|msg://x":                               {},
			}
			wantSize = 3
		case registry.ArrayModeRemove:
			value = []registry.Link{{Kind: "repo", Target: "https://r1.invalid"}}
			wantSet = map[string]struct{}{"team_lead|msg://agent/agent-mux/agt_lead000001": {}}
			wantSize = 1
		}
		got, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Links:         &registry.ArrayPatch[registry.Link]{Mode: mode, Value: value},
			LastUpdatedBy: "t",
		})
		if err != nil {
			t.Fatalf("UpdateSelf: %v", err)
		}
		if len(got.Links) != wantSize {
			t.Fatalf("links count = %d, want %d (%v)",
				len(got.Links), wantSize, linkKeys(got.Links))
		}
		for _, l := range got.Links {
			k := l.Kind + "|" + l.Target
			if _, ok := wantSet[k]; !ok {
				t.Errorf("unexpected link %q in result", k)
			}
		}
	})

	t.Run("empty-no-op", func(t *testing.T) {
		before, _ := svc.Lookup(ctx, reg.URN)
		got, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Links:         &registry.ArrayPatch[registry.Link]{Mode: mode, Value: nil},
			LastUpdatedBy: "t",
		})
		if err != nil {
			t.Fatalf("UpdateSelf: %v", err)
		}
		if len(got.Links) != len(before.Links) {
			t.Errorf("empty Value should be no-op: got %d links, want %d",
				len(got.Links), len(before.Links))
		}
	})
}

// ─── UpdateSelf concurrent ───────────────────────────────────────────────────

// TestService_UpdateSelf_Concurrent fires two goroutines that update
// different scalar columns on the same URN. The MaxOpenConns=1 pool
// serializes them; the final reload should reflect both updates with
// no lost writes. Run under -race in CI.
func TestService_UpdateSelf_Concurrent(t *testing.T) {
	t.Parallel()
	svc := newServiceForConcurrency(t)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Concurrent"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	titleVal := "NewTitle"
	roleVal := "NewRole"

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Title:         &titleVal,
			LastUpdatedBy: "writer-a",
		}); err != nil {
			t.Errorf("writer-a UpdateSelf: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := svc.UpdateSelf(ctx, reg.URN, registry.UpdatePatch{
			Role:          &roleVal,
			LastUpdatedBy: "writer-b",
		}); err != nil {
			t.Errorf("writer-b UpdateSelf: %v", err)
		}
	}()
	wg.Wait()

	got, err := svc.Lookup(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Title != "NewTitle" {
		t.Errorf("Title = %q, want NewTitle (writer-a's value)", got.Title)
	}
	if got.Role != "NewRole" {
		t.Errorf("Role = %q, want NewRole (writer-b's value)", got.Role)
	}
}

// ─── Deregister ──────────────────────────────────────────────────────────────

func TestService_Deregister_HappyPreservesChildTables(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	learned := fixedTime(t, "2026-05-01T00:00:00Z")

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName:  "ToRetire",
		Capabilities: []string{"go", "rust"},
		Skills: []registry.Skill{
			{Name: "design", LearnedAt: learned},
		},
		Links: []registry.Link{
			{Kind: "repo", Target: "https://r.invalid"},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := svc.Deregister(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if got.Status != registry.StatusDeprecated {
		t.Errorf("Status = %q, want deprecated", got.Status)
	}
	// Load-bearing T-02 invariant: child tables intact after soft-delete.
	if !stringSetEqual(got.Capabilities, []string{"go", "rust"}) {
		t.Errorf("Capabilities = %v, want preserved [go rust]", got.Capabilities)
	}
	if len(got.Skills) != 1 || got.Skills[0].Name != "design" {
		t.Errorf("Skills = %v, want preserved [design]", skillNames(got.Skills))
	}
	if len(got.Links) != 1 {
		t.Errorf("Links count = %d, want 1 (preserved)", len(got.Links))
	}
}

func TestService_Deregister_UnknownURN(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	_, err := svc.Deregister(ctx, "msg://agent/agent-mux/agt_missing0099")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ─── Sync ────────────────────────────────────────────────────────────────────

// newServiceWithResolvers returns a Service backed by an in-memory SQLite
// + a FileResolver rooted at root + a CLIResolver. Used by Sync tests.
func newServiceWithResolvers(t *testing.T, root string) *registry.Service {
	t.Helper()
	storage := newStorage(t)
	fr, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	return registry.NewService(storage,
		registry.WithResolver(fr),
		registry.WithResolver(registry.NewCLIResolver()))
}

func TestService_Sync_FileResolverHappyPath(t *testing.T) {
	root := canonTempDir(t)
	target := filepath.Join(root, "agent.json")
	body := []byte(`{
		"display_name": "Synced Alpha",
		"role": "implementer-after-sync",
		"title": "Engineer",
		"capabilities": ["go", "rust"],
		"links": [{"kind": "repo", "target": "https://r.invalid"}]
	}`)
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	svc := newServiceWithResolvers(t, root)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Pre-Sync Name",
		Role:        "pre-sync",
		Callback: &registry.Callback{
			Scheme: "file",
			Target: "file://" + target,
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	before := reg.CachedAt

	got, err := svc.Sync(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.DisplayName != "Synced Alpha" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Synced Alpha")
	}
	if got.Role != "implementer-after-sync" {
		t.Errorf("Role = %q, want %q", got.Role, "implementer-after-sync")
	}
	if got.Title != "Engineer" {
		t.Errorf("Title = %q, want %q", got.Title, "Engineer")
	}
	if !stringSetEqual(got.Capabilities, []string{"go", "rust"}) {
		t.Errorf("Capabilities = %v, want [go rust]", got.Capabilities)
	}
	if len(got.Links) != 1 || got.Links[0].Kind != "repo" {
		t.Errorf("Links = %v, want one repo link", got.Links)
	}
	if got.LastUpdatedBy != "system:sync" {
		t.Errorf("LastUpdatedBy = %q, want system:sync", got.LastUpdatedBy)
	}
	if got.CachedAt == nil {
		t.Fatal("CachedAt = nil, want bumped value")
	}
	if before != nil && !got.CachedAt.After(*before) {
		t.Errorf("CachedAt not bumped: before=%v after=%v", *before, *got.CachedAt)
	}
}

func TestService_Sync_CLIResolverHappyPath(t *testing.T) {
	root := canonTempDir(t)
	svc := newServiceWithResolvers(t, root)
	ctx := context.Background()

	// Single-token printf payload (no quoting v1).
	payload := `{"display_name":"CLI-Synced","title":"FromCLI"}`
	cmd := "cli://printf " + payload

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "PreCLI",
		Callback:    &registry.Callback{Scheme: "cli", Target: cmd},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := svc.Sync(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.DisplayName != "CLI-Synced" {
		t.Errorf("DisplayName = %q, want CLI-Synced", got.DisplayName)
	}
	if got.Title != "FromCLI" {
		t.Errorf("Title = %q, want FromCLI", got.Title)
	}
}

func TestService_Sync_NoCallback(t *testing.T) {
	root := canonTempDir(t)
	svc := newServiceWithResolvers(t, root)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Naked",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = svc.Sync(ctx, reg.URN)
	if !errors.Is(err, registry.ErrNoCallback) {
		t.Errorf("err = %v, want ErrNoCallback", err)
	}
}

func TestService_Sync_NoResolverForScheme(t *testing.T) {
	// Build a service WITHOUT any resolvers registered, then point a row
	// at a file:// callback. Sync should surface ErrNoResolver.
	storage := newStorage(t)
	svc := registry.NewService(storage)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Stranded",
		Callback:    &registry.Callback{Scheme: "file", Target: "file:///nowhere"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = svc.Sync(ctx, reg.URN)
	if !errors.Is(err, registry.ErrNoResolver) {
		t.Errorf("err = %v, want ErrNoResolver", err)
	}
}

func TestService_Sync_MalformedJSONPayload(t *testing.T) {
	root := canonTempDir(t)
	target := filepath.Join(root, "broken.json")
	if err := os.WriteFile(target, []byte(`{ "display_name": broken`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	svc := newServiceWithResolvers(t, root)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Before",
		Callback:    &registry.Callback{Scheme: "file", Target: "file://" + target},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = svc.Sync(ctx, reg.URN)
	if !errors.Is(err, registry.ErrPayloadInvalid) {
		t.Errorf("err = %v, want ErrPayloadInvalid", err)
	}
}

func TestService_Sync_YAMLPayload(t *testing.T) {
	root := canonTempDir(t)
	target := filepath.Join(root, "agent.yaml")
	body := []byte("display_name: YAML Alpha\nrole: yamler\ntitle: YAMLWriter\n")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	svc := newServiceWithResolvers(t, root)
	ctx := context.Background()

	reg, err := svc.Register(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Before",
		Callback:    &registry.Callback{Scheme: "file", Target: "file://" + target},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := svc.Sync(ctx, reg.URN)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got.DisplayName != "YAML Alpha" {
		t.Errorf("DisplayName = %q, want YAML Alpha", got.DisplayName)
	}
	if got.Role != "yamler" {
		t.Errorf("Role = %q, want yamler", got.Role)
	}
	if got.Title != "YAMLWriter" {
		t.Errorf("Title = %q, want YAMLWriter", got.Title)
	}
}

func TestService_Sync_UnknownURN(t *testing.T) {
	root := canonTempDir(t)
	svc := newServiceWithResolvers(t, root)
	ctx := context.Background()
	_, err := svc.Sync(ctx, "msg://agent/agent-mux/agt_absent00099")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ─── v060-05 T-02: group registry service ────────────────────────────────────

// newServiceWithStorage exposes both *Service and the backing *Storage so
// group tests can pre-seed group_members rows (moderator/member fixtures)
// before exercising service-level archive/role checks.
func newServiceWithStorage(t *testing.T) (*registry.Service, *registry.Storage) {
	t.Helper()
	s := newStorage(t)
	return registry.NewService(s), s
}

// registerCreator is a small fixture helper — Register a no-frills agent
// to act as the creator/owner URN in group tests.
func registerCreator(t *testing.T, svc *registry.Service, display string) registry.Profile {
	t.Helper()
	got, err := svc.Register(context.Background(), registry.KindAgent, registry.Profile{
		DisplayName: display,
	})
	if err != nil {
		t.Fatalf("register creator: %v", err)
	}
	return got
}

func TestService_RegisterGroup_HappyPath(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	ctx := context.Background()
	creator := registerCreator(t, svc, "Creator")

	got, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName:   "Design Room",
		Description:   "Multi-agent design sync",
		Role:          "design-room",
		Capabilities:  []string{"design", "v060"},
		LastUpdatedBy: creator.URN,
	})
	if err != nil {
		t.Fatalf("Register group: %v", err)
	}
	if !registry.IsGroupURN(got.URN) {
		t.Errorf("URN = %q; want a group URN", got.URN)
	}
	if got.Kind != registry.KindGroup {
		t.Errorf("Kind = %q; want %q", got.Kind, registry.KindGroup)
	}
	if got.Status != registry.StatusActive {
		t.Errorf("Status = %q; want %q", got.Status, registry.StatusActive)
	}
	if got.DisplayName != "Design Room" {
		t.Errorf("DisplayName = %q; want %q", got.DisplayName, "Design Room")
	}
	if len(got.Capabilities) != 2 {
		t.Errorf("Capabilities = %v; want 2", got.Capabilities)
	}

	role, ok, err := storage.GroupMemberRole(ctx, got.URN, creator.URN)
	if err != nil {
		t.Fatalf("GroupMemberRole: %v", err)
	}
	if !ok {
		t.Fatal("creator not present in group_members")
	}
	if role != registry.MemberRoleOwner {
		t.Errorf("creator role = %q; want %q", role, registry.MemberRoleOwner)
	}
}

func TestService_RegisterGroup_RequiresCreator(t *testing.T) {
	svc := newService(t)
	_, err := svc.Register(context.Background(), registry.KindGroup, registry.Profile{
		DisplayName: "Anonymous Group",
		// LastUpdatedBy missing on purpose
	})
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_RegisterGroup_RejectsMalformedCreatorURN(t *testing.T) {
	svc := newService(t)
	_, err := svc.Register(context.Background(), registry.KindGroup, registry.Profile{
		DisplayName:   "Group",
		LastUpdatedBy: "not-a-urn",
	})
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_RegisterGroup_RejectsUnknownCreator(t *testing.T) {
	svc := newService(t)
	_, err := svc.Register(context.Background(), registry.KindGroup, registry.Profile{
		DisplayName:   "Group",
		LastUpdatedBy: "msg://agent/agent-mux/agt_doesnotexist",
	})
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_RegisterGroup_RejectsGroupCreator(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	creator := registerCreator(t, svc, "Creator")
	parent, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName:   "Parent Group",
		LastUpdatedBy: creator.URN,
	})
	if err != nil {
		t.Fatalf("register parent group: %v", err)
	}
	// Now try to register a group with the parent group as creator — rejected.
	_, err = svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName:   "Child Group",
		LastUpdatedBy: parent.URN,
	})
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_ListGroupsForMember_MultipleGroups(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	ctx := context.Background()
	owner := registerCreator(t, svc, "Owner")
	other := registerCreator(t, svc, "Other Member")

	for _, name := range []string{"Alpha Room", "Beta Room", "Gamma Room"} {
		g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
			DisplayName:   name,
			LastUpdatedBy: owner.URN,
		})
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		// Add `other` as a plain member of Beta Room only.
		if name == "Beta Room" {
			if err := storage.InsertGroupMember(ctx, g.URN, other.URN, registry.MemberRoleMember, time.Now()); err != nil {
				t.Fatalf("seed Beta member: %v", err)
			}
		}
	}

	got, err := svc.ListGroupsForMember(ctx, owner.URN)
	if err != nil {
		t.Fatalf("ListGroupsForMember(owner): %v", err)
	}
	if len(got) != 3 {
		t.Errorf("owner groups = %d; want 3", len(got))
	}
	// Alphabetical order.
	wantOrder := []string{"Alpha Room", "Beta Room", "Gamma Room"}
	for i, p := range got {
		if p.DisplayName != wantOrder[i] {
			t.Errorf("groups[%d] = %q; want %q", i, p.DisplayName, wantOrder[i])
		}
	}

	gotOther, err := svc.ListGroupsForMember(ctx, other.URN)
	if err != nil {
		t.Fatalf("ListGroupsForMember(other): %v", err)
	}
	if len(gotOther) != 1 || gotOther[0].DisplayName != "Beta Room" {
		t.Errorf("other groups = %v; want [Beta Room]", gotOther)
	}
}

func TestService_ListGroupsForMember_NonMember(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	owner := registerCreator(t, svc, "Owner")
	stranger := registerCreator(t, svc, "Stranger")
	if _, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "Private", LastUpdatedBy: owner.URN,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := svc.ListGroupsForMember(ctx, stranger.URN)
	if err != nil {
		t.Fatalf("ListGroupsForMember: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("stranger groups = %v; want empty", got)
	}
}

func TestService_ListGroupsForMember_RejectsBadURN(t *testing.T) {
	svc := newService(t)
	_, err := svc.ListGroupsForMember(context.Background(), "garbage")
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_ArchiveGroup_OwnerSucceeds(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	owner := registerCreator(t, svc, "Owner")
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "To Archive", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := svc.ArchiveGroup(ctx, g.URN, owner.URN)
	if err != nil {
		t.Fatalf("ArchiveGroup: %v", err)
	}
	if got.Status != registry.StatusArchived {
		t.Errorf("Status = %q; want %q", got.Status, registry.StatusArchived)
	}
}

func TestService_ArchiveGroup_ModeratorSucceeds(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	ctx := context.Background()
	owner := registerCreator(t, svc, "Owner")
	mod := registerCreator(t, svc, "Moderator")
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "Mod Test", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := storage.InsertGroupMember(ctx, g.URN, mod.URN, registry.MemberRoleModerator, time.Now()); err != nil {
		t.Fatalf("seed moderator: %v", err)
	}
	if _, err := svc.ArchiveGroup(ctx, g.URN, mod.URN); err != nil {
		t.Fatalf("ArchiveGroup by moderator: %v", err)
	}
}

func TestService_ArchiveGroup_MemberForbidden(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	ctx := context.Background()
	owner := registerCreator(t, svc, "Owner")
	member := registerCreator(t, svc, "Plain Member")
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "Member Test", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := storage.InsertGroupMember(ctx, g.URN, member.URN, registry.MemberRoleMember, time.Now()); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	_, err = svc.ArchiveGroup(ctx, g.URN, member.URN)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_ArchiveGroup_NonMemberForbidden(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	owner := registerCreator(t, svc, "Owner")
	stranger := registerCreator(t, svc, "Stranger")
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "Closed Room", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err = svc.ArchiveGroup(ctx, g.URN, stranger.URN)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_ArchiveGroup_RejectsAgentURN(t *testing.T) {
	svc := newService(t)
	owner := registerCreator(t, svc, "Owner")
	_, err := svc.ArchiveGroup(context.Background(), owner.URN, owner.URN)
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_ArchiveGroup_NotFound(t *testing.T) {
	svc := newService(t)
	owner := registerCreator(t, svc, "Owner")
	_, err := svc.ArchiveGroup(context.Background(), "msg://group/agent-mux/grp_doesnotexist", owner.URN)
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

// ─── v060-05 T-03: membership service ────────────────────────────────────────

// groupFixture is a helper that registers an owner + a group and returns
// (group, owner). Used as the starting point for most membership tests.
func groupFixture(t *testing.T, svc *registry.Service, groupName string) (registry.Profile, registry.Profile) {
	t.Helper()
	owner := registerCreator(t, svc, "Owner-"+groupName)
	g, err := svc.Register(context.Background(), registry.KindGroup, registry.Profile{
		DisplayName:   groupName,
		LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register group %q: %v", groupName, err)
	}
	return g, owner
}

func TestService_AddMember_HappyPath(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Add-OK")
	newMember := registerCreator(t, svc, "Newbie")

	got, err := svc.AddMember(ctx, g.URN, newMember.URN, owner.URN, registry.MemberRoleMember)
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if got.Role != registry.MemberRoleMember {
		t.Errorf("Role = %q; want %q", got.Role, registry.MemberRoleMember)
	}
	if got.GroupURN != g.URN {
		t.Errorf("GroupURN = %q; want %q", got.GroupURN, g.URN)
	}
}

func TestService_AddMember_RejectsNonModerator(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, _ := groupFixture(t, svc, "Add-Forbid")
	stranger := registerCreator(t, svc, "Stranger")
	candidate := registerCreator(t, svc, "Candidate")

	_, err := svc.AddMember(ctx, g.URN, candidate.URN, stranger.URN, registry.MemberRoleMember)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_AddMember_RejectsUnknownMember(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Add-Unknown")
	_, err := svc.AddMember(ctx, g.URN, "msg://agent/agent-mux/agt_xxxxxxxxxx", owner.URN, registry.MemberRoleMember)
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

func TestService_AddMember_RejectsDuplicate(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Add-Dup")
	newMember := registerCreator(t, svc, "Dup")
	if _, err := svc.AddMember(ctx, g.URN, newMember.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("first add: %v", err)
	}
	_, err := svc.AddMember(ctx, g.URN, newMember.URN, owner.URN, registry.MemberRoleMember)
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_RemoveMember_HappyPath(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Remove-OK")
	target := registerCreator(t, svc, "Target")
	if _, err := svc.AddMember(ctx, g.URN, target.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := svc.RemoveMember(ctx, g.URN, target.URN, owner.URN); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	members, err := svc.ListMembers(ctx, g.URN)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 1 || members[0].MemberURN != owner.URN {
		t.Errorf("members after remove = %v; want only the owner", members)
	}
}

func TestService_RemoveMember_CannotRemoveOwner(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Remove-Owner")
	// Promote a second member to moderator (so byURN passes the role check)
	// and have them try to remove the owner.
	mod := registerCreator(t, svc, "Moderator")
	if _, err := svc.AddMember(ctx, g.URN, mod.URN, owner.URN, registry.MemberRoleModerator); err != nil {
		t.Fatalf("seed mod: %v", err)
	}
	err := svc.RemoveMember(ctx, g.URN, owner.URN, mod.URN)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_LeaveGroup_MemberSelfRemoves(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Leave-Member")
	mem := registerCreator(t, svc, "Leaver")
	if _, err := svc.AddMember(ctx, g.URN, mem.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.LeaveGroup(ctx, g.URN, mem.URN); err != nil {
		t.Fatalf("LeaveGroup: %v", err)
	}
}

func TestService_LeaveGroup_OwnerWithModeratorSucceeds(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Leave-Owner-OK")
	mod := registerCreator(t, svc, "Successor")
	if _, err := svc.AddMember(ctx, g.URN, mod.URN, owner.URN, registry.MemberRoleModerator); err != nil {
		t.Fatalf("seed mod: %v", err)
	}
	if err := svc.LeaveGroup(ctx, g.URN, owner.URN); err != nil {
		t.Fatalf("LeaveGroup owner with mod: %v", err)
	}
}

func TestService_LeaveGroup_OwnerWithoutModForbidden(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Leave-Owner-Forbid")
	// Add only a plain member, no moderator/owner successor.
	mem := registerCreator(t, svc, "Just A Member")
	if _, err := svc.AddMember(ctx, g.URN, mem.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := svc.LeaveGroup(ctx, g.URN, owner.URN)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_SetMemberRole_OwnerPromotesToModerator(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "SetRole-Owner-Mod")
	target := registerCreator(t, svc, "Promotee")
	if _, err := svc.AddMember(ctx, g.URN, target.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.SetMemberRole(ctx, g.URN, target.URN, registry.MemberRoleModerator, owner.URN); err != nil {
		t.Fatalf("SetMemberRole: %v", err)
	}
	members, err := svc.ListMembers(ctx, g.URN)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var found bool
	for _, m := range members {
		if m.MemberURN == target.URN && m.Role == registry.MemberRoleModerator {
			found = true
		}
	}
	if !found {
		t.Errorf("target not promoted; members = %v", members)
	}
}

func TestService_SetMemberRole_ModeratorCannotPromoteToOwner(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "SetRole-Mod-Owner")
	mod := registerCreator(t, svc, "Mod")
	target := registerCreator(t, svc, "Promotee")
	if _, err := svc.AddMember(ctx, g.URN, mod.URN, owner.URN, registry.MemberRoleModerator); err != nil {
		t.Fatalf("seed mod: %v", err)
	}
	if _, err := svc.AddMember(ctx, g.URN, target.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	err := svc.SetMemberRole(ctx, g.URN, target.URN, registry.MemberRoleOwner, mod.URN)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_SetMemberRole_NonAuthByForbidden(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "SetRole-NoAuth")
	stranger := registerCreator(t, svc, "Stranger")
	target := registerCreator(t, svc, "Promotee")
	if _, err := svc.AddMember(ctx, g.URN, target.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := svc.SetMemberRole(ctx, g.URN, target.URN, registry.MemberRoleModerator, stranger.URN)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_ListMembers_OrderedByJoinedAt(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "List-Order")
	// Add three more members in order; verify display_name + ordering.
	for i, name := range []string{"Beta", "Charlie", "Delta"} {
		m := registerCreator(t, svc, name)
		if _, err := svc.AddMember(ctx, g.URN, m.URN, owner.URN, registry.MemberRoleMember); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		// Tiny sleep to ensure joined_at ordering is stable (sqlite has
		// 1s formatTime resolution otherwise).
		time.Sleep(2 * time.Millisecond)
	}
	members, err := svc.ListMembers(ctx, g.URN)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 4 {
		t.Fatalf("members = %d; want 4", len(members))
	}
	// First is the owner, then Beta/Charlie/Delta in join order.
	wantNames := []string{"Owner-List-Order", "Beta", "Charlie", "Delta"}
	for i, m := range members {
		if m.DisplayName != wantNames[i] {
			t.Errorf("members[%d].DisplayName = %q; want %q", i, m.DisplayName, wantNames[i])
		}
	}
}

func TestService_ListMembers_RejectsAgentURN(t *testing.T) {
	svc := newService(t)
	owner := registerCreator(t, svc, "Owner")
	_, err := svc.ListMembers(context.Background(), owner.URN)
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

// ─── v060-05 T-04: group messaging service ───────────────────────────────────

func TestService_SendToGroup_HappyPath(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Send-OK")

	gm, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "application/json", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("SendToGroup: %v", err)
	}
	if gm.GroupSeq != 1 {
		t.Errorf("GroupSeq = %d; want 1", gm.GroupSeq)
	}
	if gm.ID == "" {
		t.Error("ID is empty")
	}
	if gm.GroupURN != g.URN {
		t.Errorf("GroupURN = %q; want %q", gm.GroupURN, g.URN)
	}
	if gm.FromURN != owner.URN {
		t.Errorf("FromURN = %q; want %q", gm.FromURN, owner.URN)
	}
}

func TestService_SendToGroup_AssignsSequentialSeqs(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Send-Seq")
	for i := 1; i <= 5; i++ {
		gm, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if gm.GroupSeq != int64(i) {
			t.Errorf("send %d: GroupSeq = %d; want %d", i, gm.GroupSeq, i)
		}
	}
}

func TestService_SendToGroup_NonMemberForbidden(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, _ := groupFixture(t, svc, "Send-NonMember")
	stranger := registerCreator(t, svc, "Stranger")
	_, err := svc.SendToGroup(ctx, g.URN, stranger.URN, "notice", "", "", nil)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_SendToGroup_ArchivedRejected(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Send-Archived")
	if _, err := svc.ArchiveGroup(ctx, g.URN, owner.URN); err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", nil)
	if !errors.Is(err, registry.ErrGroupArchived) {
		t.Fatalf("err = %v; want ErrGroupArchived", err)
	}
}

func TestService_SendToGroup_RejectsAgentURN(t *testing.T) {
	svc := newService(t)
	owner := registerCreator(t, svc, "Owner")
	_, err := svc.SendToGroup(context.Background(), owner.URN, owner.URN, "notice", "", "", nil)
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

// TestService_SendToGroup_ConcurrentSeqAssignment verifies the MAX+1
// counter under concurrent sends produces a contiguous 1..N sequence
// with no duplicates and no skips (D10 — per-group monotonic counter).
// Uses newServiceForConcurrency to pin MaxOpenConns=1 (the production
// invariant that makes MAX+1 race-safe via connection-pool
// serialization).
func TestService_SendToGroup_ConcurrentSeqAssignment(t *testing.T) {
	svc := newServiceForConcurrency(t)
	ctx := context.Background()
	owner, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "Concurrent", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}

	const N = 20
	var wg sync.WaitGroup
	seqs := make(chan int64, N)
	errs := make(chan error, N)
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gm, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`))
			if err != nil {
				errs <- err
				return
			}
			seqs <- gm.GroupSeq
		}()
	}
	wg.Wait()
	close(seqs)
	close(errs)
	for e := range errs {
		t.Fatalf("send error: %v", e)
	}
	got := make([]int64, 0, N)
	for s := range seqs {
		got = append(got, s)
	}
	if len(got) != N {
		t.Fatalf("got %d seqs; want %d", len(got), N)
	}
	seen := make(map[int64]bool, N)
	for _, s := range got {
		if s < 1 || s > int64(N) {
			t.Errorf("seq %d outside [1, %d]", s, N)
		}
		if seen[s] {
			t.Errorf("duplicate seq %d", s)
		}
		seen[s] = true
	}
	for i := int64(1); i <= int64(N); i++ {
		if !seen[i] {
			t.Errorf("missing seq %d", i)
		}
	}
}

func TestService_ListGroupMessages_HappyPath(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "List-OK")
	for i := 0; i < 3; i++ {
		if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	msgs, err := svc.ListGroupMessages(ctx, g.URN, owner.URN, -1, "", 100)
	if err != nil {
		t.Fatalf("ListGroupMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %d; want 3", len(msgs))
	}
	for i, m := range msgs {
		if m.GroupSeq != int64(i+1) {
			t.Errorf("messages[%d].GroupSeq = %d; want %d", i, m.GroupSeq, i+1)
		}
	}
}

func TestService_ListGroupMessages_RespectsJoinedAtGate(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "List-JoinedGate")
	// Owner sends 2 messages before adding a new member.
	for i := 0; i < 2; i++ {
		if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("pre-add send %d: %v", i, err)
		}
	}
	// Ensure timestamp resolution boundary: sleep so joined_at strictly
	// exceeds the pre-send created_at values.
	time.Sleep(5 * time.Millisecond)
	newMember := registerCreator(t, svc, "Latecomer")
	if _, err := svc.AddMember(ctx, g.URN, newMember.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	// Owner sends 1 more after the new member joined.
	if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("post-add send: %v", err)
	}
	msgs, err := svc.ListGroupMessages(ctx, g.URN, newMember.URN, -1, "", 100)
	if err != nil {
		t.Fatalf("ListGroupMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("late member messages = %d; want 1 (only post-join)", len(msgs))
	}
	if msgs[0].GroupSeq != 3 {
		t.Errorf("seq = %d; want 3", msgs[0].GroupSeq)
	}
}

func TestService_ListGroupMessages_SinceSeqZeroUsesCursor(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "List-Cursor")
	for i := 0; i < 5; i++ {
		if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	// Mark read up to 3. sinceSeq=0 should now substitute 3 → return seqs 4,5.
	if err := svc.MarkRead(ctx, g.URN, owner.URN, 3); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	msgs, err := svc.ListGroupMessages(ctx, g.URN, owner.URN, 0, "", 100)
	if err != nil {
		t.Fatalf("ListGroupMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d; want 2", len(msgs))
	}
	wantSeqs := []int64{4, 5}
	for i, m := range msgs {
		if m.GroupSeq != wantSeqs[i] {
			t.Errorf("messages[%d].GroupSeq = %d; want %d", i, m.GroupSeq, wantSeqs[i])
		}
	}
}

func TestService_ListGroupMessages_ThreadFilter(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "List-Thread")
	if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "thread-a", "", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("send A1: %v", err)
	}
	if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "thread-b", "", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("send B: %v", err)
	}
	if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "thread-a", "", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("send A2: %v", err)
	}
	msgs, err := svc.ListGroupMessages(ctx, g.URN, owner.URN, -1, "thread-a", 100)
	if err != nil {
		t.Fatalf("ListGroupMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("thread-a messages = %d; want 2", len(msgs))
	}
}

func TestService_ListGroupMessages_NonMemberForbidden(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, _ := groupFixture(t, svc, "List-NonMember")
	stranger := registerCreator(t, svc, "Stranger")
	_, err := svc.ListGroupMessages(ctx, g.URN, stranger.URN, -1, "", 100)
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("err = %v; want ErrForbidden", err)
	}
}

func TestService_MarkRead_Monotonic(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "MarkRead-Mono")
	for i := 0; i < 5; i++ {
		if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	if err := svc.MarkRead(ctx, g.URN, owner.URN, 4); err != nil {
		t.Fatalf("MarkRead 4: %v", err)
	}
	// Smaller upToSeq is a no-op.
	if err := svc.MarkRead(ctx, g.URN, owner.URN, 1); err != nil {
		t.Fatalf("MarkRead 1: %v", err)
	}
	// Cursor should still be 4. Reading from sinceSeq=0 returns only seq=5.
	msgs, err := svc.ListGroupMessages(ctx, g.URN, owner.URN, 0, "", 100)
	if err != nil {
		t.Fatalf("ListGroupMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].GroupSeq != 5 {
		t.Errorf("after monotonic MarkRead, messages = %v; want one seq=5", msgs)
	}
}

func TestService_MarkRead_RejectsNegative(t *testing.T) {
	svc := newService(t)
	owner := registerCreator(t, svc, "Owner")
	g, err := svc.Register(context.Background(), registry.KindGroup, registry.Profile{
		DisplayName: "G", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	err = svc.MarkRead(context.Background(), g.URN, owner.URN, -1)
	if !errors.Is(err, registry.ErrInvalidRequest) {
		t.Fatalf("err = %v; want ErrInvalidRequest", err)
	}
}

func TestService_GetMyMentions_FiltersByGroupKey(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	ctx := context.Background()
	owner := registerCreator(t, svc, "Owner")
	// Inject two notice envelopes directly via Storage.InsertGroupMessage —
	// one with payload.group set (a mention notice), one without (plain
	// notice). GetMyMentions should return only the first.
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{DisplayName: "Notif", LastUpdatedBy: owner.URN})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	if _, err := storage.InsertGroupMessage(ctx, owner.URN, "msg://system/mention",
		"notice", "", "application/json",
		json.RawMessage(`{"group":"`+g.URN+`","message_id":"01H","seq":1}`)); err != nil {
		t.Fatalf("insert mention notice: %v", err)
	}
	if _, err := storage.InsertGroupMessage(ctx, owner.URN, "msg://system/mention",
		"notice", "", "application/json",
		json.RawMessage(`{"text":"non-mention"}`)); err != nil {
		t.Fatalf("insert plain notice: %v", err)
	}
	got, err := svc.GetMyMentions(ctx, owner.URN, time.Time{}, 50)
	if err != nil {
		t.Fatalf("GetMyMentions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("mentions = %d; want 1", len(got))
	}
}

// fakeMentionParser is a recording stub used by the hook-invocation tests.
// Parse runs pre-commit (can fail SendToGroup); Dispatch runs post-commit
// (fire-and-forget).
type fakeMentionParser struct {
	mu            sync.Mutex
	parseCalls    int
	dispatchCalls []registry.GroupMessage
	parseErr      error
	parseMentions []registry.Mention
}

func (f *fakeMentionParser) Parse(_ context.Context, _ json.RawMessage, _ string) ([]registry.Mention, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parseCalls++
	if f.parseErr != nil {
		return nil, f.parseErr
	}
	return f.parseMentions, nil
}

func (f *fakeMentionParser) Dispatch(_ context.Context, gm registry.GroupMessage, _ []registry.Mention) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatchCalls = append(f.dispatchCalls, gm)
}

func TestService_SendToGroup_InvokesMentionParser(t *testing.T) {
	storage := newStorage(t)
	parser := &fakeMentionParser{
		parseMentions: []registry.Mention{{Token: "everyone", ResolvedURN: "msg://agent/agent-mux/agt_xxxxxxxxxx", Source: "display_name"}},
	}
	svc := registry.NewService(storage, registry.WithMentionParser(parser))
	ctx := context.Background()
	owner, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "Hook Test", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{"text":"hi @everyone"}`)); err != nil {
		t.Fatalf("SendToGroup: %v", err)
	}
	parser.mu.Lock()
	defer parser.mu.Unlock()
	if parser.parseCalls != 1 {
		t.Errorf("Parse calls = %d; want 1", parser.parseCalls)
	}
	if len(parser.dispatchCalls) != 1 {
		t.Fatalf("Dispatch calls = %d; want 1", len(parser.dispatchCalls))
	}
	if parser.dispatchCalls[0].GroupURN != g.URN {
		t.Errorf("dispatched GroupURN %q; want %q", parser.dispatchCalls[0].GroupURN, g.URN)
	}
}

func TestService_SendToGroup_AmbiguousMentionAbortsBeforeCommit(t *testing.T) {
	storage := newStorage(t)
	parser := &fakeMentionParser{
		parseErr: &registry.ErrAmbiguousMention{Token: "alex", Candidates: []string{
			"msg://agent/agent-mux/agt_a1", "msg://agent/agent-mux/agt_b2",
		}},
	}
	svc := registry.NewService(storage, registry.WithMentionParser(parser))
	ctx := context.Background()
	owner, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	g, err := svc.Register(ctx, registry.KindGroup, registry.Profile{
		DisplayName: "Ambig", LastUpdatedBy: owner.URN,
	})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	_, err = svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{"text":"hi @alex"}`))
	var ambErr *registry.ErrAmbiguousMention
	if !errors.As(err, &ambErr) {
		t.Fatalf("err = %v; want *ErrAmbiguousMention", err)
	}
	if ambErr.Token != "alex" {
		t.Errorf("Token = %q; want %q", ambErr.Token, "alex")
	}
	// Dispatch must NOT have fired (Parse failed pre-commit).
	parser.mu.Lock()
	defer parser.mu.Unlock()
	if len(parser.dispatchCalls) != 0 {
		t.Errorf("Dispatch called %d times; want 0 (Parse failed)", len(parser.dispatchCalls))
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func skillNames(skills []registry.Skill) []string {
	out := make([]string, len(skills))
	for i, s := range skills {
		out[i] = s.Name
	}
	return out
}

func linkKeys(links []registry.Link) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.Kind + "|" + l.Target
	}
	return out
}
