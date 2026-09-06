package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func openTestStoreForSessions(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSessionProviderMapping_UpsertAndGet(t *testing.T) {
	db := openTestStoreForSessions(t)
	row := SessionRow{ID: "s1", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if err := db.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A mapping can be recorded before any native id is known.
	if err := db.UpsertSessionProviderMapping("s1", "tether", "opencode", ""); err != nil {
		t.Fatalf("upsert (no native id): %v", err)
	}
	got, err := db.GetSessionProviderMapping("s1", "tether", "opencode")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.NativeSessionID.Valid {
		t.Fatalf("expected no native id yet, got %+v", got.NativeSessionID)
	}
	firstUpdatedAt := got.UpdatedAt

	// A later observation resolves the native id -- updates in place,
	// preserves created_at, does not create a second row.
	if err := db.UpsertSessionProviderMapping("s1", "tether", "opencode", "native-123"); err != nil {
		t.Fatalf("upsert (with native id): %v", err)
	}
	got, err = db.GetSessionProviderMapping("s1", "tether", "opencode")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if !got.NativeSessionID.Valid || got.NativeSessionID.String != "native-123" {
		t.Fatalf("expected native id native-123, got %+v", got.NativeSessionID)
	}
	if got.CreatedAt == "" {
		t.Fatalf("expected created_at to be preserved, got empty")
	}
	_ = firstUpdatedAt

	all, err := db.ListSessionProviderMappings("s1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly one mapping row (upsert must not duplicate), got %d: %+v", len(all), all)
	}
}

func TestSessionProviderMapping_DistinctProvidersDoNotCollide(t *testing.T) {
	db := openTestStoreForSessions(t)
	row := SessionRow{ID: "s1", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if err := db.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Two different provider kinds observed on the SAME session must land in
	// two distinct rows -- this is the exact bug the shared, single-valued
	// logical_agents.claude_session_id column has today (T02 research: it is
	// overwritten last-write-wins regardless of which provider wrote it).
	if err := db.UpsertSessionProviderMapping("s1", "tether", "claude-code", "claude-native-1"); err != nil {
		t.Fatalf("upsert claude-code: %v", err)
	}
	if err := db.UpsertSessionProviderMapping("s1", "tether", "opencode", "opencode-native-1"); err != nil {
		t.Fatalf("upsert opencode: %v", err)
	}

	all, err := db.ListSessionProviderMappings("s1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 distinct provider mappings, got %d: %+v", len(all), all)
	}

	claude, err := db.GetSessionProviderMapping("s1", "tether", "claude-code")
	if err != nil {
		t.Fatalf("get claude-code: %v", err)
	}
	if claude.NativeSessionID.String != "claude-native-1" {
		t.Fatalf("claude-code native id clobbered: %+v", claude)
	}
	oc, err := db.GetSessionProviderMapping("s1", "tether", "opencode")
	if err != nil {
		t.Fatalf("get opencode: %v", err)
	}
	if oc.NativeSessionID.String != "opencode-native-1" {
		t.Fatalf("opencode native id clobbered: %+v", oc)
	}
}

func TestSessionProviderMapping_NotFound(t *testing.T) {
	db := openTestStoreForSessions(t)
	_, err := db.GetSessionProviderMapping("nope", "tether", "claude-code")
	if !errors.Is(err, ErrProviderMappingNotFound) {
		t.Fatalf("expected ErrProviderMappingNotFound, got %v", err)
	}
}

func TestSessionProviderMapping_RequiresKeyFields(t *testing.T) {
	db := openTestStoreForSessions(t)
	if err := db.UpsertSessionProviderMapping("", "tether", "claude-code", "x"); err == nil {
		t.Fatalf("expected error for empty sessionID")
	}
	if err := db.UpsertSessionProviderMapping("s1", "", "claude-code", "x"); err == nil {
		t.Fatalf("expected error for empty owner")
	}
	if err := db.UpsertSessionProviderMapping("s1", "tether", "", "x"); err == nil {
		t.Fatalf("expected error for empty provider")
	}
}

func TestCreateSession_CanonicalIdentityFields(t *testing.T) {
	db := openTestStoreForSessions(t)
	fresh := SessionRow{ID: "s1", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if err := db.CreateSession(fresh, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatalf("create fresh: %v", err)
	}
	got, err := db.GetSession("s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Intent != "fresh" {
		t.Fatalf("expected default intent 'fresh', got %q", got.Intent)
	}
	if got.Publication != "private-local" {
		t.Fatalf("expected default publication 'private-local', got %q", got.Publication)
	}
	if got.ParentSessionID.Valid {
		t.Fatalf("expected no parent session id on a fresh boot, got %+v", got.ParentSessionID)
	}

	resumed := SessionRow{
		ID: "s2", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv",
		Workspace: "/tmp/ws", State: "created",
		Intent:          "resume",
		ParentSessionID: sql.NullString{String: "s1", Valid: true},
	}
	if err := db.CreateSession(resumed, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatalf("create resumed: %v", err)
	}
	gotResumed, err := db.GetSession("s2")
	if err != nil {
		t.Fatalf("get resumed: %v", err)
	}
	if gotResumed.Intent != "resume" {
		t.Fatalf("expected intent 'resume', got %q", gotResumed.Intent)
	}
	if !gotResumed.ParentSessionID.Valid || gotResumed.ParentSessionID.String != "s1" {
		t.Fatalf("expected parent_session_id=s1, got %+v", gotResumed.ParentSessionID)
	}

	// Old session remains independently resolvable -- resuming into a new
	// row never mutates or removes the prior one.
	original, err := db.GetSession("s1")
	if err != nil {
		t.Fatalf("original session must remain resolvable after resume: %v", err)
	}
	if original.Intent != "fresh" {
		t.Fatalf("resuming must not rewrite the parent session's own intent, got %q", original.Intent)
	}
}
