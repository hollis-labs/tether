package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDaemonUILineageIsAuthenticatedItemOnlyAndNeverNative(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateSession(store.SessionRow{ID: "source", LogicalAgentID: "seat", WorkerID: "worker", State: "completed"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: "child", LogicalAgentID: "seat", WorkerID: "worker", Intent: "resume", ParentSessionID: sql.NullString{String: "source", Valid: true}, State: "launching"}, &launch.Plan{Continuity: "relaunch"}); err != nil {
		t.Fatal(err)
	}
	ack := store.WorkerLaunchAcknowledgement{SessionID: "child", WorkerID: "worker", CommandID: "launch:child", ReplyKind: "launched", ReplySessionID: "child", ReplyCommandID: "launch:child"}
	if err := db.RecordWorkerLineageAck(context.Background(), ack); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionState("child", "running", 7, nil); err != nil {
		t.Fatal(err)
	}
	ids := identity.NewStore(db.DB())
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "reader", Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{Store: db, Manager: agentsessions.NewManager(nil)}
	h := identity.MiddlewareWithScopes(identity.Enforce, ids, nil, identity.ScopePolicy{}, api.NewHandler(api.Deps{Service: &serviceAdapter{svc: svc}, GroupStore: db}))
	for _, name := range []string{"local", "remote", "historical", "unauthenticated", "collection"} {
		t.Run(name, func(t *testing.T) {
			path := "/sessions/child"
			if name == "historical" {
				path = "/sessions/source"
			}
			if name == "collection" {
				path = "/sessions"
			}
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if name != "unauthenticated" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			if name == "remote" {
				r = r.WithContext(identity.WithRemoteContext(r.Context()))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if name == "unauthenticated" {
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("unauthenticated=%d", w.Code)
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("read=%d %s", w.Code, w.Body.String())
			}
			if name == "collection" {
				var list api.ListSessionsResponse
				if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
					t.Fatal(err)
				}
				for _, row := range list.Sessions {
					if row.WorkerLineage != nil || row.ReplacementLineage != nil || row.NativeAdmission != nil {
						t.Fatal("collection populated detail evidence")
					}
				}
				return
			}
			var row api.SessionDTO
			if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
				t.Fatal(err)
			}
			if row.NativeAdmission != nil || row.ReplacementLineage != nil {
				t.Fatal("worker UI lineage became native/team proof")
			}
			if name == "historical" {
				if row.WorkerLineage != nil {
					t.Fatal("historical source promoted")
				}
				return
			}
			if row.WorkerLineage == nil || row.WorkerLineage.Kind != "worker_lineage_v1" || row.WorkerLineage.CommandID != "launch:child" || row.WorkerLineage.SourceSessionID != "source" {
				t.Fatalf("item proof lost: %s", w.Body.String())
			}
			raw, _ := json.Marshal(row.WorkerLineage)
			for _, private := range []string{"credential", "workroot", "native_thread", "custody", "binding_secret"} {
				if strings.Contains(string(raw), private) {
					t.Fatalf("UI proof leaked %s", private)
				}
			}
		})
	}
}
