package mcpadapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func creatorService(t *testing.T) *app.Service {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root, repo, sessions := t.TempDir(), t.TempDir(), t.TempDir()
	files := map[string]string{
		"global.yaml":             "version: 0.1.0\ncatalog:\n  defaults:\n    state_db: " + filepath.Join(root, "state.db") + "\n",
		"projects/p.yaml":         "id: p\nrepo_root: " + repo + "\nworkspace:\n  default_mode: shared\n  session_root: " + sessions + "\n",
		"providers/fill.yaml":     "id: fill-cli\ntype: cli\nprovider: claude\nruntime_kind: pty\ncommand: " + filepath.Join(root, "fake-provider") + "\n",
		"agents/fill-worker.yaml": "id: fill-worker\nname: Fill Worker\n",
		"launches/fill.yaml":      "id: fill-launch\nproject: p\nagent: fill-worker\nprovider: fill-cli\n",
		"fake-provider":           "#!/bin/sh\nexec sleep 60\n",
		"providers/stub.yaml":     "id: api-stub\ntype: api\n",
		"agents/worker.yaml":      "id: worker\nname: Worker\n",
		"launches/l1.yaml":        "id: l1\nproject: p\nagent: worker\nprovider: api-stub\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "fake-provider"), 0700); err != nil {
		t.Fatal(err)
	}
	svc, err := app.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestNativeSessionCreateKeepsVerifiedCreatorAndKeyScope(t *testing.T) {
	svc := creatorService(t)
	ids := map[string]string{}
	for _, caller := range []string{"creator-a", "creator-b", "creator-a"} {
		p := identity.Principal{ID: caller, Kind: "operator", Verified: true, Scopes: []string{ScopeSessionWrite}}
		a, err := NewVerifiedAdapter(identity.WithPrincipal(context.Background(), p), svc, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.SessionID = "forged-session"
		ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "forged-dispatch", Verified: true})
		out, err := a.handleSessionCreate(ctx, map[string]any{"launch_id": "l1", "idempotency_key": "shared", "idempotency_principal": "forged-argument"})
		if err != nil {
			t.Fatal(err)
		}
		id := out.(map[string]any)["session_id"].(string)
		row, err := svc.Store.GetSession(id)
		if err != nil || row.RequestingPrincipal.String != caller {
			t.Fatalf("creator=%+v %v", row, err)
		}
		if prior := ids[caller]; prior != "" && prior != id {
			t.Fatal("same creator/key did not replay")
		}
		ids[caller] = id
		// Bare create takes the same provenance path, without relying on a key.
		bare, err := a.handleSessionCreate(ctx, map[string]any{"launch_id": "l1"})
		if err != nil {
			t.Fatal(err)
		}
		row, err = svc.Store.GetSession(bare.(map[string]any)["session_id"].(string))
		if err != nil || row.RequestingPrincipal.String != caller {
			t.Fatalf("bare creator=%+v %v", row, err)
		}
	}
	if ids["creator-a"] == ids["creator-b"] {
		t.Fatal("different verified creators shared a key space")
	}
}

func TestNativeSessionCreateDoesNotTrustUnverifiedIdentity(t *testing.T) {
	svc := creatorService(t)
	a := New(svc, "legacy-label", []string{ScopeSessionWrite})
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "unverified-label"})
	out, err := a.handleSessionCreate(ctx, map[string]any{"launch_id": "l1", "requesting_principal": "forged"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession(out.(map[string]any)["session_id"].(string))
	if err != nil || row.RequestingPrincipal.String != store.IdempotencyPrincipalAnonymous {
		t.Fatalf("unverified creator=%+v %v", row, err)
	}
}

func TestNativeLogicalResumeRecordsCreatorBeforeLaunchRefusal(t *testing.T) {
	svc := creatorService(t)
	source, err := svc.CreateSession("l1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.UpdateSessionState(source.SessionID, "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	p := identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}}
	ctx := identity.WithPrincipal(context.Background(), p)
	a, err := NewVerifiedAdapter(ctx, svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The in-process reference provider is intentionally unsupported by the
	// shared native launch matrix. Stop at that refusal: this tests the request
	// provenance persisted before admission, without running a provider process.
	_, err = a.handleLogicalAgentResume(ctx, map[string]any{"logical_agent_id": "worker", "idempotency_key": "resume", "idempotency_principal": "forged"})
	if err == nil || !strings.Contains(err.Error(), "unknown provider id") {
		t.Fatalf("expected reference-provider refusal, got %v", err)
	}
	rows, err := svc.Store.ListSessions(store.ListSessionsOptions{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("resume allocation=%+v %v", rows, err)
	}
	found := false
	for _, row := range rows {
		if row.ParentSessionID.String != source.SessionID {
			continue
		}
		found = true
		if row.RequestingPrincipal.String != p.ID || row.Intent != "resume" {
			t.Fatalf("resume creator=%+v", row)
		}
	}
	if !found {
		t.Fatal("resume did not retain its source")
	}
}

func TestNativeSessionCreateNeverFallsBackFromRequiredWorker(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, configured, code string
		require                          bool
	}{
		{name: "required but missing", require: true, code: "worker_not_configured"},
		{name: "explicit unavailable", explicit: "missing-worker", code: "worker_unavailable"},
		{name: "catalog default unavailable", configured: "missing-worker", require: true, code: "worker_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := creatorService(t)
			if tc.configured != "" {
				f, err := os.OpenFile(filepath.Join(svc.CatalogRoot, "launches/l1.yaml"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("worker: " + tc.configured + "\n")
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("fixture: %v %v", err, closeErr)
				}
			}
			a := New(svc, "legacy", []string{ScopeSessionWrite})
			_, err := a.handleSessionCreate(context.Background(), map[string]any{"launch_id": "l1", "continuity": "relaunch", "worker": tc.explicit, "require_worker": tc.require})
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("worker refusal=%v, want %s", err, tc.code)
			}
			rows, listErr := svc.Store.ListSessions(store.ListSessionsOptions{})
			if listErr != nil || len(rows) != 0 {
				t.Fatalf("worker refusal allocated hub session: %+v %v", rows, listErr)
			}
		})
	}
}
