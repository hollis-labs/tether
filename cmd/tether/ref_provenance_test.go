package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app/sessionrefs"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

type refTestLaunchService struct{ api.LaunchService }

// Legacy --daemon-only capture (including the worker SSH route) uses this
// HTTP sink. Even successful capture cannot manufacture hub-observed proxy refs.
func TestRefAttacherDerivesSourceFromSessionBearer(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "refs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CreateSession(store.SessionRow{ID: "s", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	ids := identity.NewStore(db.DB())
	expires := time.Now().Add(time.Hour)
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://session/local/s", Kind: "session", SessionID: "s", Scopes: []string{"session.write"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(api.Deps{Service: &refTestLaunchService{}, SessionRefs: db})
	srv := httptest.NewServer(identity.Middleware(identity.Enforce, ids, nil, h))
	defer srv.Close()
	dc := client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(token))
	capture := sessionrefs.New(true, "s", nil, refAttacherClient{c: dc}, nil)
	failed := sessionrefs.Observation{ToolName: "torque_task_get", Arguments: map[string]any{"id": "CW-20261016-0158"}, Failed: true}
	success := sessionrefs.Observation{ToolName: "torque_task_get", Arguments: map[string]any{"id": "CW-20261016-0157"}}
	capture.Record(context.Background(), failed)
	capture.Record(context.Background(), success)
	capture.Record(context.Background(), success)
	rows, err := db.ListSessionRefs("s", store.ListSessionRefsOptions{})
	if err != nil || len(rows) != 1 || rows[0].Source != store.SourceSession || rows[0].RefID != "CW-20261016-0157" || rows[0].Relation != store.RelationRead {
		t.Fatalf("capture=%+v err=%v", rows, err)
	}
	if _, err := dc.AttachSessionRef(context.Background(), "s", api.SessionRefAttachRequest{Kind: "torque_task", RefID: "spoof", Source: "proxy"}); err == nil {
		t.Fatal("bearer could assert proxy")
	}
	rows, err = db.ListSessionRefs("s", store.ListSessionRefsOptions{})
	if err != nil || len(rows) != 1 {
		t.Fatal("spoof persisted", err)
	}
}
