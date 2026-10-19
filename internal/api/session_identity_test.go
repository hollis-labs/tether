package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

type identityReadService struct {
	*fakeLaunchService
	read func([]string) (map[string][]store.SessionProviderMapping, error)
}

func (s identityReadService) SessionProviderMappings(ids []string) (map[string][]store.SessionProviderMapping, error) {
	return s.read(ids)
}

func TestSessionIdentityReads(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSessionGroup(store.SessionGroupRow{ID: "g", CreatedAt: "2026-10-19T00:00:00Z", UpdatedAt: "2026-10-19T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	row := store.SessionRow{ID: "child", LaunchID: "not-a-native-id", Workspace: "/private/workspace", ProviderID: "codex", State: "created", ParentSessionID: sql.NullString{String: "parent", Valid: true}, Intent: "resume", TaskID: sql.NullString{String: "CW-20261018-0049", Valid: true}, WorkerID: "worker-1"}
	if err := db.CreateSession(row, &launch.Plan{LaunchID: row.LaunchID}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSessionToGroup(row.ID, "g"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ owner, provider, id string }{{"tether", "codex", "shared"}, {"other", "codex", "shared"}, {"tether", "claude", ""}} {
		if err := db.UpsertSessionProviderMapping(row.ID, m.owner, m.provider, m.id); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := db.GetSession(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	svc := identityReadService{fakeLaunchService: &fakeLaunchService{listRes: []store.SessionRow{*stored}, getRes: map[string]*store.SessionRow{row.ID: stored}}, read: func(ids []string) (map[string][]store.SessionProviderMapping, error) {
		reads++
		return db.SessionProviderMappings(ids)
	}}
	for _, path := range []string{"/sessions", "/sessions/child", "/session-groups/g/sessions"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(identity.WithRemoteContext(req.Context()))
			rr := httptest.NewRecorder()
			NewHandler(Deps{Service: svc, GroupStore: db}).ServeHTTP(rr, req)
			if rr.Code != 200 {
				t.Fatalf("%d: %s", rr.Code, rr.Body.String())
			}
			var dto SessionDTO
			if path != "/sessions/child" {
				var page ListSessionsResponse
				if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if len(page.Sessions) != 1 {
					t.Fatalf("expected one session: %s", rr.Body.String())
				}
				dto = page.Sessions[0]
			} else if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
				t.Fatal(err)
			}
			if dto.ParentSessionID != "parent" || dto.Intent != "resume" || dto.TaskID != row.TaskID.String || dto.WorkerID != "worker-1" {
				t.Fatalf("lost explicit relations: %+v", dto)
			}
			if len(dto.ProviderMappings) != 3 {
				t.Fatalf("mappings: %+v", dto.ProviderMappings)
			}
			if dto.ProviderMappings[0].NativeSessionID != nil {
				t.Fatal("unknown native ID was inferred")
			}
			a, b := dto.ProviderMappings[1], dto.ProviderMappings[2]
			if a.Owner == b.Owner || *a.NativeSessionID != "shared" || *b.NativeSessionID != "shared" {
				t.Fatal("native ID qualifiers lost")
			}
			if strings.Contains(rr.Body.String(), row.Workspace) {
				t.Fatal("remote workspace path leaked")
			}
		})
	}
	if reads != 3 {
		t.Fatalf("%d lookups for three requests", reads)
	}
}

func TestSessionIdentityAbsentAndUnavailable(t *testing.T) {
	row := store.SessionRow{ID: "s", LaunchID: "native-looking", Workspace: "/native-looking"}
	for _, available := range []bool{false, true} {
		var svc LaunchService = &fakeLaunchService{getRes: map[string]*store.SessionRow{"s": &row}}
		if available {
			svc = identityReadService{fakeLaunchService: svc.(*fakeLaunchService), read: func([]string) (map[string][]store.SessionProviderMapping, error) { return nil, nil }}
		}
		rr := httptest.NewRecorder()
		newTestHandler(svc).ServeHTTP(rr, httptest.NewRequest("GET", "/sessions/s", nil))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := "null"
		if available {
			want = "[]"
		}
		if string(body["provider_mappings"]) != want {
			t.Fatalf("mappings = %s want %s", body["provider_mappings"], want)
		}
		for _, key := range []string{"parent_session_id", "intent", "task_id", "worker_id"} {
			if _, ok := body[key]; ok {
				t.Fatalf("inferred %s", key)
			}
		}
	}
}

func TestSessionIdentityLookupError(t *testing.T) {
	svc := identityReadService{fakeLaunchService: &fakeLaunchService{listRes: []store.SessionRow{{ID: "s"}}, getRes: map[string]*store.SessionRow{"s": {ID: "s"}}}, read: func([]string) (map[string][]store.SessionProviderMapping, error) {
		return nil, errors.New("/private/store.db lookup failed")
	}}
	for _, path := range []string{"/sessions", "/sessions/s"} {
		rr := httptest.NewRecorder()
		newTestHandler(svc).ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 500 || strings.Contains(rr.Body.String(), "/private") {
			t.Fatalf("unsafe error: %d %s", rr.Code, rr.Body.String())
		}
	}
}
