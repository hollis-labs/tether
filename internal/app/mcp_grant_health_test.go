package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func TestMCPGrantPreflightHealthAndNotice(t *testing.T) {
	ctx := context.Background()
	svc, root := reloadService(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	svc.Store = db
	p := mcpgateway.SessionPolicy{SessionID: "running", AgentID: "agent", Servers: []string{"removed", "disabled", "keep"}}.Seal()
	for _, state := range []string{"running", "completed"} {
		id := state
		if err := db.CreateSession(store.SessionRow{ID: id, LogicalAgentID: "agent", State: "running"}, &launch.Plan{}); err != nil {
			t.Fatal(err)
		}
		policy := p
		policy.SessionID = id
		policy = policy.Seal()
		if err := db.SaveSessionMCPPolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
		if state == "completed" {
			if _, err := db.DB().Exec(`UPDATE sessions SET state='completed' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeCat(t, root, "mcp-servers/keep.yaml", "id: keep\ntransport: http\nurl: http://localhost:12345/mcp\n")
	writeCat(t, root, "mcp-servers/disabled.yaml", "id: disabled\nenabled: false\ntransport: http\nurl: http://localhost:12345/mcp\n")
	candidate, err := config.LoadLayered(root)
	if err != nil {
		t.Fatal(err)
	}
	impacts, err := svc.MCPGrantImpacts(ctx, candidate)
	if err != nil || len(impacts) != 1 || len(impacts[0].Servers) != 2 {
		t.Fatalf("impacts=%+v err=%v", impacts, err)
	}
	report, err := svc.ReloadCatalog(ctx)
	if err != nil || len(report.MCPGrantImpacts) != 1 {
		t.Fatalf("reload=%+v err=%v", report, err)
	}
	health := svc.MCPGrantHealth(ctx, "running")
	if health == nil || len(health.Servers) != 2 {
		t.Fatalf("health=%+v", health)
	}
	notify := svc.MCPGrantNotifier()
	notify(ctx, p, health.Servers)
	notify(ctx, p, health.Servers)
	to, err := messaging.ParseURN(registry.LogicalAgentBindingTarget("agent"))
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := db.MessagingStore().Inbox(ctx, to, messaging.Filter{})
	if err != nil || len(inbox) != 1 || !strings.Contains(string(inbox[0].Payload), "mcp_grant_stale") || !strings.Contains(strings.ToLower(string(inbox[0].Payload)), "resume or relaunch") {
		t.Fatalf("notice=%+v %v", inbox, err)
	}
	stored, err := db.SessionMCPPolicy(ctx, "running")
	if err != nil || stored.Digest != p.Digest {
		t.Fatal("snapshot changed", err)
	}
}
