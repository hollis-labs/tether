package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// The allowlist is saved with the session policy, comes back identical, and
// cannot be replaced once the policy is bound to a credential.
func TestSessionMCPPolicyKeepsEditableAgents(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.CreateSession(SessionRow{ID: "s1", LogicalAgentID: "agent", State: "created"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	saved := mcpgateway.SessionPolicy{SessionID: "s1", AgentID: "agent", Servers: []string{"one"}, EditableAgents: []string{"worker", "reviewer"}}.Seal()
	if err := db.SaveSessionMCPPolicy(ctx, saved); err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionMCPPolicy(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.EditableAgents, []string{"reviewer", "worker"}) || got.Digest != saved.Digest {
		t.Fatalf("round trip lost the allowlist: %+v", got)
	}
	// repeating the same snapshot is fine; a different allowlist is a different policy
	if err := db.SaveSessionMCPPolicy(ctx, saved); err != nil {
		t.Fatalf("repeat of the same snapshot refused: %v", err)
	}
	widened := mcpgateway.SessionPolicy{SessionID: "s1", AgentID: "agent", Servers: []string{"one"}}.Seal()
	if err := db.SaveSessionMCPPolicy(ctx, widened); err == nil {
		t.Fatal("a bound policy was replaced by one without the allowlist")
	}
}
