package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

func TestSessionMCPPolicy_ImmutableAndActiveOnly(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.CreateSession(SessionRow{ID: "s", LogicalAgentID: "agent", State: "created"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SessionMCPPolicy(ctx, "s"); !errors.Is(err, ErrSessionMCPPolicyUnavailable) {
		t.Fatal("legacy policy inferred", err)
	}
	policy := mcpgateway.SessionPolicy{SessionID: "s", AgentID: "agent", Servers: []string{}}.Seal()
	if err := db.SaveSessionMCPPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSessionMCPPolicy(ctx, policy); err != nil {
		t.Fatal("identical preparation retry failed", err)
	}
	wider := policy
	wider.Servers = []string{"new"}
	if err := db.SaveSessionMCPPolicy(ctx, wider.Seal()); err == nil {
		t.Fatal("bound grant widened")
	}
	stored, err := db.SessionMCPPolicy(ctx, "s")
	if err != nil || stored.Servers == nil || len(stored.Servers) != 0 {
		t.Fatalf("explicit zero lost: %+v %v", stored, err)
	}
	if err := db.UpdateSessionState("s", "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SessionMCPPolicy(ctx, "s"); !errors.Is(err, ErrSessionMCPPolicyUnavailable) {
		t.Fatal("terminal policy accepted", err)
	}
	if err := db.SaveSessionMCPPolicy(ctx, policy); err == nil {
		t.Fatal("terminal snapshot saved")
	}
}

func TestSessionMCPPolicy_CannotBindOtherAgentOrSession(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateSession(SessionRow{ID: "s", LogicalAgentID: "agent", State: "created"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []mcpgateway.SessionPolicy{{SessionID: "s", AgentID: "other", Servers: []string{}}, {SessionID: "missing", AgentID: "agent", Servers: []string{}}} {
		if err := db.SaveSessionMCPPolicy(context.Background(), p.Seal()); err == nil {
			t.Fatal("cross-agent/session policy saved")
		}
	}
}
