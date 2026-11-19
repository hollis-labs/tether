package conversation

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestResolverByConversationAndBySession(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mk := func(id, parent, worker string) {
		t.Helper()
		row := store.SessionRow{ID: id, LaunchID: "l", ProjectID: "p", LogicalAgentID: "seat", ProviderID: "pv", Workspace: "/tmp/ws", State: "created", WorkerID: worker}
		if parent != "" {
			row.Intent, row.ParentSessionID = "resume", sql.NullString{String: parent, Valid: true}
		}
		if err := st.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
			t.Fatal(err)
		}
	}
	r := Resolver{Store: st}
	if _, err := r.ByConversation(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing conversation: %v", err)
	}
	if _, _, err := r.BySession(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	mk("s1", "", "")
	first, current, err := r.BySession(ctx, "s1")
	if err != nil || !current || first.Generation != 1 || first.SessionID != "s1" || first.AttemptID == "" || first.Seat != "seat" || first.Agent != "seat" || first.Kind != "stable" {
		t.Fatalf("first = %+v current=%v err=%v", first, current, err)
	}
	mk("s2", "s1", "worker-1")
	got, err := r.ByConversation(ctx, first.ConversationID)
	if err != nil || got.Generation != 2 || got.SessionID != "s2" || got.WorkerID != "worker-1" || got.AttemptID == first.AttemptID {
		t.Fatalf("after resume = %+v err=%v", got, err)
	}
	if bySeat, err := r.BySeat(ctx, "seat"); err != nil || bySeat != got {
		t.Fatalf("BySeat = %+v, %v; want %+v", bySeat, err, got)
	}
	if _, err := r.BySeat(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown seat: %v", err)
	}
	old, current, err := r.BySession(ctx, "s1")
	if err != nil || current || old != got {
		t.Fatalf("superseded session = %+v current=%v err=%v; want %+v, not current", old, current, err, got)
	}
	if _, err := r.Resolve(ctx, "s1"); !errors.Is(err, ErrStale) {
		t.Fatalf("Resolve(superseded session) = %v, want ErrStale", err)
	}
	for _, target := range []string{first.ConversationID, "s2"} {
		if res, err := r.Resolve(ctx, target); err != nil || res != got {
			t.Fatalf("Resolve(%s) = %+v, %v; want %+v", target, res, err, got)
		}
	}
	if _, err := r.Resolve(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(missing) = %v", err)
	}
}
