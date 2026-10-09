package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

func TestRecoveryMCPPolicyRequiresRetainedAuthority(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"valid", ""},
		{"revoked principal", "UPDATE principals SET revoked_at='revoked'"},
		{"expired principal", "UPDATE principals SET expires_at='2000-01-01T00:00:00Z'"},
		{"revoked binding", "UPDATE runtime_bindings SET revoked_at='revoked'"},
		{"expired binding", "UPDATE runtime_bindings SET lease_expires_at='2000-01-01T00:00:00Z'"},
		{"superseded binding", "INSERT INTO runtime_bindings(id,target_urn,session_id,host_id,attempt_id,generation,leased_at,created_at,updated_at,revoked_at) VALUES('new','actor','other','team','new',2,'now','now','now','revoked')"},
		{"terminal session", "UPDATE sessions SET state='failed' WHERE id='s'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := context.Background()
			for _, id := range []string{"s", "other"} {
				if err = db.CreateSession(SessionRow{ID: id, LogicalAgentID: "agent", State: "created"}, &launch.Plan{}); err != nil {
					t.Fatal(err)
				}
			}
			if err = db.SaveSessionMCPPolicy(ctx, mcpgateway.SessionPolicy{SessionID: "s", AgentID: "agent", Servers: []string{"engine"}}.Seal()); err != nil {
				t.Fatal(err)
			}
			if _, err = db.RecoverySessionMCPPolicy(ctx, "s"); !errors.Is(err, ErrSessionMCPPolicyUnavailable) {
				t.Fatal("snapshot inferred authority", err)
			}
			for _, query := range []string{
				`INSERT INTO principals(principal_id,kind,display,token_hash,scopes_json,session_id,addresses_json,created_at) VALUES('p','session','fixture','0123456789012345678901234567890123456789012345678901234567890123','[]','s','[]','now')`,
				`INSERT INTO runtime_bindings(id,target_urn,session_id,host_id,attempt_id,generation,leased_at,created_at,updated_at) VALUES('b','actor','s','team','original',1,'now','now','now')`, tc.change,
			} {
				if query != "" {
					if _, err = db.DB().Exec(query); err != nil {
						t.Fatal(err)
					}
				}
			}
			policy, err := db.RecoverySessionMCPPolicy(ctx, "s")
			if tc.change == "" {
				if err != nil || policy.SessionID != "s" {
					t.Fatal("valid retained policy refused", err)
				}
			} else if !errors.Is(err, ErrSessionMCPPolicyUnavailable) {
				t.Fatal("invalid retained authority accepted", err)
			}
		})
	}
}
