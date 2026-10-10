package mcpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
)

func TestEnvironmentDirectoryMCPRequiresDaemon(t *testing.T) {
	a := &Adapter{svc: &app.Service{}}
	ctx := context.Background()
	for _, handler := range []func(context.Context, map[string]any) (any, error){a.handleEnvironmentList, a.handleEnvironmentGet, a.handleEnvironmentRegister, a.handleEnvironmentRename, a.handleEnvironmentRemove} {
		if _, err := handler(ctx, map[string]any{"environment_id": "id"}); err == nil {
			t.Fatal("in-process fallback accepted")
		}
	}
}

func TestEnvironmentDirectoryMCPPreservesDaemonRefusal(t *testing.T) {
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Error("ambient test credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			_, _ = w.Write([]byte(`{"environments":[]}`))
			return
		}
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"verified local operator required"}}`))
	}))
	defer server.Close()
	a := &Adapter{client: client.New("tcp:"+server.Listener.Addr().String(), client.WithToken("")), scopes: map[string]struct{}{"admin": {}, "*": {}}}
	if _, err := a.handleEnvironmentList(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.handleEnvironmentRemove(context.Background(), map[string]any{"environment_id": "id"}); err == nil {
		t.Fatal("MCP widened caller")
	}
	if calls[len(calls)-1] != "DELETE /environments/id" {
		t.Fatal(calls)
	}
	before := len(calls)
	if _, err := a.handleEnvironmentRegister(context.Background(), map[string]any{"registration": map[string]any{"authority": "worker", "grant": "admin"}}); err == nil || len(calls) != before {
		t.Fatal("unknown registration field reached daemon")
	}
}
