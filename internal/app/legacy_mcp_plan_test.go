package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestLaunchSessionRejectsPersistedLegacyMCPGrants(t *testing.T) {
	for _, key := range []string{"MUX_MCP_SERVERS", "MUX_MCP_DENY", "AGENT_MUX_MCP_SCOPES"} {
		t.Run(key, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			plan := &launch.Plan{ProviderID: "must-not-run", Env: map[string]string{key: "private-grant-value"}}
			if err := db.CreateSession(store.SessionRow{ID: "created-before-cutover", State: "created", ProviderID: plan.ProviderID}, plan); err != nil {
				t.Fatal(err)
			}
			svc := &Service{Store: db}
			_, err = svc.LaunchSession("created-before-cutover")
			if err == nil || !strings.Contains(err.Error(), "recreate the session") || !strings.Contains(err.Error(), key) {
				t.Fatalf("expected legacy-plan refusal before runtime lookup, got %v", err)
			}
			if strings.Contains(err.Error(), "private-grant-value") {
				t.Fatal("error disclosed grant value")
			}
			row, err := db.GetSession("created-before-cutover")
			if err != nil {
				t.Fatal(err)
			}
			if row.State != "created" {
				t.Fatalf("state = %s", row.State)
			}
		})
	}
}

func TestLegacyMCPPlanUsesOnlyExplicitCurrentEquivalent(t *testing.T) {
	for _, env := range []map[string]string{
		{"TETHER_MCP_SERVERS": ""},
		{"MUX_MCP_SERVERS": "legacy", "TETHER_MCP_SERVERS": ""},
		{"AGENT_MUX_MCP_SCOPES": "legacy", "TETHER_MCP_SCOPES": "current"},
		{"MUX_UNRELATED": "value"},
	} {
		if err := rejectLegacyMCPPlan(&launch.Plan{Env: env}); err != nil {
			t.Fatal(err)
		}
	}
}
