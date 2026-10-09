package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

func TestNativeOnlyCredentialNeverWidensOriginalCeiling(t *testing.T) {
	for _, kind := range []string{"bounded", "auto_revoked_source", "missing_current", "expired_source", "revoked_current", "missing_source", "narrower_current", "foreign_source_principal", "foreign_source_only"} {
		t.Run(kind, func(t *testing.T) {
			r := recoveryRuntimeRig(t, nil)
			r.plan.NativeResumeOnly = true
			if _, err := r.service.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.native_resume_only',json('true')) WHERE session_id='resumed'`); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().UTC().Add(30 * time.Minute)
			ids := identity.NewStore(r.service.Store.DB())
			token, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://session/local/source", Kind: "session", SessionID: "source", Scopes: []string{"session.write", "message.write"}, ExpiresAt: &deadline})
			if err != nil {
				t.Fatal(err)
			}
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
			switch kind {
			case "foreign_source_principal", "foreign_source_only":
				if err := ids.RevokeToken(ctx, token); err != nil {
					t.Fatal(err)
				}
				if _, err := ids.Mint(ctx, identity.Principal{ID: "zz-foreign-session-principal", Kind: "session", SessionID: "source", Scopes: []string{"*"}}); err != nil {
					t.Fatal(err)
				}
				if kind == "foreign_source_only" {
					if _, err := r.service.Store.DB().Exec(`DELETE FROM principals WHERE principal_id='msg://session/local/source'`); err != nil {
						t.Fatal(err)
					}
				}
			case "auto_revoked_source":
				if err := ids.RevokeToken(ctx, token); err != nil {
					t.Fatal(err)
				}
			case "missing_source":
				if _, err := r.service.Store.DB().Exec(`DELETE FROM principals WHERE session_id='source'`); err != nil {
					t.Fatal(err)
				}
			case "narrower_current":
				at := time.Now().UTC().Add(10 * time.Minute)
				ctx = identity.WithPrincipal(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"session.write"}, ExpiresAt: &at})
			case "missing_current":
				ctx = context.Background()
			case "expired_source":
				if _, err := r.service.Store.DB().Exec(`UPDATE principals SET expires_at=? WHERE session_id='source'`, time.Now().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
			case "revoked_current":
				now := time.Now()
				ctx = identity.WithPrincipal(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}, RevokedAt: &now})
			}
			minted, err := r.service.mintSessionCredential(ctx, "resumed")
			positive := kind == "bounded" || kind == "auto_revoked_source" || kind == "narrower_current" || kind == "foreign_source_principal"
			if !positive {
				if minted != "" || !errors.Is(err, ErrNativeOnlyUnavailable) {
					t.Fatalf("unsafe mint accepted: %v", err)
				}
				var count int
				if err := r.service.Store.DB().QueryRow(`SELECT count(*) FROM principals WHERE session_id='resumed'`).Scan(&count); err != nil || count != 0 {
					t.Fatal("refusal minted credential", err)
				}
				return
			}
			if err != nil || minted == "" {
				t.Fatalf("bounded mint refused: %v", err)
			}
			var raw, expiry string
			if err := r.service.Store.DB().QueryRow(`SELECT scopes_json,expires_at FROM principals WHERE session_id='resumed'`).Scan(&raw, &expiry); err != nil {
				t.Fatal(err)
			}
			var scopes []string
			expected := []string{"session.write", "message.write"}
			if kind == "narrower_current" {
				expected = []string{"session.write"}
			}
			if json.Unmarshal([]byte(raw), &scopes) != nil || !slices.Equal(scopes, expected) {
				t.Fatal("original scope ceiling widened")
			}
			at, err := time.Parse(time.RFC3339Nano, expiry)
			if err != nil || at.After(deadline) {
				t.Fatal("original expiry ceiling extended", err)
			}
		})
	}
}

func TestNativeOnlyPolicyPreservesSealedFloorOrRefuses(t *testing.T) {
	r := recoveryRuntimeRig(t, nil)
	source := mcpgateway.SessionPolicy{SessionID: "source", AgentID: "agent", Servers: []string{"engine"}}.Seal()
	if err := r.service.Store.SaveSessionMCPPolicy(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	current := source
	current.SessionID = "resumed"
	accepted, err := r.service.nativeOnlyPolicy(context.Background(), r.plan, current.Seal())
	if err != nil || accepted.SessionID != "resumed" || !slices.Equal(accepted.Servers, source.Servers) {
		t.Fatal("source floor not preserved", err)
	}
	for _, servers := range [][]string{{"engine", "foreign"}, {}} {
		current.Servers = servers
		if _, err := r.service.nativeOnlyPolicy(context.Background(), r.plan, current.Seal()); !errors.Is(err, ErrNativeOnlyUnavailable) {
			t.Fatal("changed policy silently substituted", err)
		}
	}
	retained, err := r.service.Store.SessionMCPPolicy(context.Background(), "source")
	if err != nil || retained.Digest != source.Digest {
		t.Fatal("historical policy rewritten", err)
	}
}
