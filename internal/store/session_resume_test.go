package store

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestClearNativeResumeDoesNotEraseNewerObservationOrOtherProvider(t *testing.T) {
	db := openTestStoreForSessions(t)
	for _, id := range []string{"parent", "child"} {
		if err := db.CreateSession(SessionRow{ID: id, LaunchID: "l", LogicalAgentID: "a", ProviderID: "codex", Workspace: "/owned", State: "failed"}, &launch.Plan{ResumeProviderSessionID: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct{ provider, id string }{{"codex", "new"}, {"claude", "claude-id"}} {
		if err := db.UpsertSessionProviderMapping("parent", "tether", entry.provider, entry.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ClearNativeResume(context.Background(), "parent", "child", "codex", "old"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ provider, id string }{{"codex", "new"}, {"claude", "claude-id"}} {
		got, err := db.GetSessionProviderMapping("parent", "tether", entry.provider)
		if err != nil || got.NativeSessionID.String != entry.id {
			t.Fatalf("mapping=%+v %v", got, err)
		}
	}
	plan, _ := db.GetLaunchPlan("child")
	if plan.ResumeProviderSessionID != "" {
		t.Fatal("failed child's resume hint retained")
	}
}
