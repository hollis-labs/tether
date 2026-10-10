package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
)

func TestEnvironmentDirectoryDaemonClient(t *testing.T) {
	methods := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/environments" && r.Method == "GET" {
			_, _ = w.Write([]byte(`{"environments":[]}`))
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		_, _ = w.Write([]byte(`{"environmentId":"id","state":"retired","revocationPending":true}`))
	}))
	defer server.Close()
	c := New("tcp:"+server.Listener.Addr().String(), WithToken(""))
	ctx := context.Background()
	if _, err := c.ListEnvironments(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetEnvironment(ctx, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterEnvironment(ctx, directory.Registration{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RenameEnvironment(ctx, "id", "label"); err != nil {
		t.Fatal(err)
	}
	out, err := c.RetireEnvironment(ctx, "id")
	if err != nil || !out.RevocationPending {
		t.Fatal("retirement not preserved", err)
	}
	want := []string{"GET /environments", "GET /environments/id", "POST /environments", "PATCH /environments/id", "DELETE /environments/id"}
	if len(methods) != len(want) {
		t.Fatalf("daemon calls %v", methods)
	}
	for i, m := range methods {
		if m != want[i] {
			t.Fatalf("call%d %s", i, m)
		}
	}
}
