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

func TestDaemonWorkerNativeAdmissionRead(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range []store.SessionRow{{ID: "source", WorkerID: "worker", LogicalAgentID: "agent", State: "completed"}, {ID: "child", WorkerID: "worker", LogicalAgentID: "agent", State: "running", Intent: "resume", ParentSessionID: sql.NullString{String: "source", Valid: true}}} {
		if err := db.CreateSession(row, &launch.Plan{NativeResumeOnly: true, ResumeSourceSessionID: "source", WorkerSourceGeneration: "g1", ResumeProviderSessionID: "thread", WorkRoot: "/private/workroot"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordWorkerNativeAdmission(store.WorkerNativeAdmission{SourceSessionID: "source", SourceGeneration: "g1", ReplacementSessionID: "child", ReplacementGeneration: "g2", WorkerID: "worker", LogicalAgentID: "agent", NativeThreadID: "thread", Workroot: "/private/workroot"}); err != nil {
		t.Fatal(err)
	}
	ids := identity.NewStore(db.DB())
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "reader", Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{Store: db, Manager: agentsessions.NewManager(nil)}
	handler := identity.MiddlewareWithScopes(identity.Enforce, ids, nil, identity.ScopePolicy{}, api.NewHandler(api.Deps{Service: &serviceAdapter{svc: svc}, GroupStore: db}))
	for _, tc := range []string{"local", "remote", "old", "unauthenticated"} {
		t.Run(tc, func(t *testing.T) {
			id := "child"
			if tc == "old" {
				id = "source"
			}
			req := httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil)
			if tc != "unauthenticated" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if tc == "remote" {
				req = req.WithContext(identity.WithRemoteContext(req.Context()))
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if tc == "unauthenticated" {
				if rr.Code != http.StatusUnauthorized {
					t.Fatalf("unauthenticated status %d", rr.Code)
				}
				return
			}
			if rr.Code != 200 {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
			var dto api.SessionDTO
			if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
				t.Fatal(err)
			}
			if tc == "old" {
				if dto.NativeAdmission != nil {
					t.Fatal("old session promoted")
				}
				return
			}
			if dto.NativeAdmission == nil || dto.NativeAdmission.SourceGeneration != "g1" || dto.NativeAdmission.ReplacementGeneration != "g2" {
				t.Fatalf("production adapter lost proof: %s", rr.Body.String())
			}
			if tc == "remote" {
				if strings.Contains(rr.Body.String(), "/private/workroot") {
					t.Fatal("remote path leaked")
				}
			} else if dto.NativeAdmission.Workroot != "/private/workroot" {
				t.Fatal("local path lost")
			}
		})
	}
}
