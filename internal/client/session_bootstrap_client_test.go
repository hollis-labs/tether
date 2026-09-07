package client

// session_bootstrap_client_test.go — end-to-end coverage for
// BootstrapSession (T08). Stands up a real internal/api.Server over a
// real *store.Store, matching bindings_client_test.go's shape.

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

func newSessionBootstrapTestClient(t *testing.T) *Client {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/bootstrap.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(api.NewHandler(api.Deps{SessionBootstrap: db}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, http: srv.Client()}
}

func TestSessionBootstrapClient_Roundtrip(t *testing.T) {
	ctx := context.Background()
	c := newSessionBootstrapTestClient(t)

	out, err := c.BootstrapSession(ctx, SessionBootstrapRequest{SessionID: "sess-1", LogicalAgentID: "agt_worker"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !out.Created || out.SessionID != "sess-1" {
		t.Fatalf("result = %+v, want created=true", out)
	}

	// Repeated call: no error, created=false.
	out2, err := c.BootstrapSession(ctx, SessionBootstrapRequest{
		SessionID: "sess-1",
		ProviderMappings: []SessionBootstrapProviderMapping{
			{Owner: "tether", Provider: "claude-code", NativeSessionID: "native-1"},
		},
	})
	if err != nil {
		t.Fatalf("repeated bootstrap: %v", err)
	}
	if out2.Created {
		t.Fatalf("result = %+v, want created=false on repeat", out2)
	}
}

func TestSessionBootstrapClient_RequiresSessionID(t *testing.T) {
	c := newSessionBootstrapTestClient(t)
	if _, err := c.BootstrapSession(context.Background(), SessionBootstrapRequest{}); err == nil {
		t.Fatal("expected an error with no session_id")
	}
}

func TestSessionBootstrapClient_Unreachable(t *testing.T) {
	srv := httptest.NewServer(nil)
	c := &Client{baseURL: srv.URL, http: srv.Client()}
	srv.Close()

	_, err := c.BootstrapSession(context.Background(), SessionBootstrapRequest{SessionID: "sess-1"})
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("got %v, want ErrDaemonUnreachable", err)
	}
}
