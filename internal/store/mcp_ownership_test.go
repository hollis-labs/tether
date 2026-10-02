package store

import (
	"context"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"path/filepath"
	"testing"
)

func TestMCPUpstreamSessionCountsUsesCapturedActiveOwnership(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for _, tc := range []struct{ id, ownership, state, provider string }{
		{"old", "", "running", "cli"}, {"legacy", "legacy_proxy", "running", "cli"},
		{"thin", "daemon", "launching", "cli"}, {"ended", "daemon", "completed", "cli"},
		{"api", "daemon", "running", "api"},
	} {
		if err := db.CreateSession(SessionRow{ID: tc.id, LogicalAgentID: "agent", State: "created", ProviderKind: tc.provider}, &launch.Plan{}); err != nil {
			t.Fatal(err)
		}
		if tc.ownership != "" {
			if err := db.SaveSessionMCPPolicy(ctx, mcpgateway.SessionPolicy{SessionID: tc.id, AgentID: "agent", UpstreamOwnership: tc.ownership}.Seal()); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.UpdateSessionState(tc.id, tc.state, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := db.MCPUpstreamSessionCounts(ctx)
	if err != nil || counts["legacy_proxy"] != 1 || counts["daemon"] != 1 || counts["unknown"] != 1 {
		t.Fatalf("captured active counts: %v %v", counts, err)
	}
}
