package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSessionCorrelationForwardedMCP(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if r.URL.Path == "/sessions" {
			w.WriteHeader(201)
		}
		_, _ = w.Write([]byte(`{"id":"s1"}`))
	}))
	defer srv.Close()
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://")), "operator", []string{ScopeSessionWrite})
	s := gomcp.NewServer("test", "0.0.1")
	a.registerSessionTools(s)
	c := connectInMemory(t, s)
	for _, name := range []string{"tether_session_create", "tether_session_launch"} {
		args := map[string]any{"task_id": "CW-1", "workstream_id": "01M7VQ2QWYB5RZR86W2BWG8PW3"}
		if name == "tether_session_create" {
			args["launch_id"] = "l1"
		} else {
			args["session_id"] = "s1"
		}
		result, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %+v", name, err, result)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests=%d", len(bodies))
	}
	for _, body := range bodies {
		if body["task_id"] != "CW-1" || body["workstream_id"] != "01M7VQ2QWYB5RZR86W2BWG8PW3" {
			t.Fatalf("lost stamps: %+v", body)
		}
	}
}

func TestNativeMCPCorrelationTrustAndReplay(t *testing.T) {
	svc := creatorService(t)
	operator := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{ScopeSessionWrite}}
	a, err := NewVerifiedAdapter(identity.WithPrincipal(context.Background(), operator), svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A forged dispatch principal must not override the authenticated view.
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "agent:forged", Kind: "agent"})
	args := map[string]any{"launch_id": "l1", "task_id": "CW-1", "workstream_id": "01M7VQ2QWYB5RZR86W2BWG8PW3", "idempotency_key": "same"}
	first, err := a.handleSessionCreate(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	id := first.(map[string]any)["session_id"].(string)
	row, err := svc.Store.GetSession(id)
	if err != nil || row.TaskID.String != "CW-1" || row.WorkstreamID.String != args["workstream_id"] {
		t.Fatalf("stamps: %+v %v", row, err)
	}
	replay, err := a.handleSessionCreate(ctx, args)
	if err != nil || replay.(map[string]any)["session_id"] != id || replay.(map[string]any)["replayed"] != true {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	args["workstream_id"] = "other"
	if _, err := a.handleSessionCreate(ctx, args); err == nil {
		t.Fatal("mismatched replay accepted")
	}
	args["idempotency_key"] = "new"
	args["workstream_id"] = " invalid "
	if _, err := a.handleSessionCreate(ctx, args); err == nil {
		t.Fatal("opaque id trimmed/accepted")
	}
	for _, p := range []identity.Principal{
		{Verified: true, ID: "agent:a", Kind: "agent"},
		{Verified: true, ID: "session:s", Kind: "session", SessionID: "s"},
		{ID: identity.OperatorID, Kind: "operator"},
		{Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "narrow"},
	} {
		p.Scopes = []string{ScopeSessionWrite}
		b, err := NewVerifiedAdapter(identity.WithPrincipal(context.Background(), p), svc, nil)
		if err != nil {
			t.Fatal(err)
		}
		forged := identity.WithPrincipal(context.Background(), operator)
		if _, err := b.handleSessionCreate(forged, map[string]any{"launch_id": "l1", "workstream_id": "work"}); err == nil {
			t.Fatal("non-operator stamped workstream")
		}
		if _, err := b.handleSessionCreate(forged, map[string]any{"launch_id": "l1", "task_id": "CW-1"}); err == nil {
			t.Fatal("non-operator stamped task")
		}
		if _, err := b.handleSessionLaunch(forged, map[string]any{"session_id": id, "workstream_id": row.WorkstreamID.String}); err == nil {
			t.Fatal("non-operator asserted launch stamp")
		}
		if _, err := b.handleSessionCreate(forged, map[string]any{"launch_id": "l1"}); err != nil {
			t.Fatalf("bare legacy create: %v", err)
		}
	}
	legacy := New(svc, "operator", []string{ScopeSessionWrite})
	if _, err := legacy.handleSessionCreate(identity.WithPrincipal(context.Background(), operator), map[string]any{"launch_id": "l1", "workstream_id": "work"}); err == nil {
		t.Fatal("legacy label/context promoted to operator")
	}
	if _, err := a.handleSessionLaunch(ctx, map[string]any{"session_id": id, "workstream_id": "other"}); err == nil {
		t.Fatal("mismatched native launch accepted")
	}
	after, err := svc.Store.GetSession(id)
	if err != nil || after.WorkstreamID != row.WorkstreamID || after.State != "created" {
		t.Fatal("assertions changed session")
	}
}
