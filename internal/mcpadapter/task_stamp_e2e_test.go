package mcpadapter

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// captureLaunches is the daemon's launch service: it records what the API handed it.
type captureLaunches struct {
	api.LaunchService
	mu     sync.Mutex
	inputs []api.CreateSessionInput
}

func (c *captureLaunches) CreateSessionWithInput(in api.CreateSessionInput) (api.LaunchResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, in)
	return api.LaunchResult{SessionID: "created-1"}, nil
}

func (c *captureLaunches) CreateSession(string) (api.LaunchResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, api.CreateSessionInput{})
	return api.LaunchResult{SessionID: "created-plain"}, nil
}

// daemonWithOperator serves the real API behind the real, enforcing identity middleware, and returns
// the operator token and an agent (session) token for it.
func daemonWithOperator(t *testing.T) (hostport, operatorToken, agentToken string, svc *captureLaunches) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ids := identity.NewStore(db.DB())
	path := filepath.Join(t.TempDir(), "operator.token")
	if err := ids.EnsureOperator(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if operatorToken, err = identity.ReadTokenFile(path); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: "s-agent", LaunchID: "l", ProjectID: "p", ProviderID: "x", ProviderKind: "cli", Workspace: "/w", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	agentToken, err = ids.Mint(context.Background(), identity.Principal{ID: "session:s-agent", Kind: "session", SessionID: "s-agent", Scopes: []string{"*"}, Addresses: []string{"session:s-agent"}})
	if err != nil {
		t.Fatal(err)
	}
	svc = &captureLaunches{}
	srv := httptest.NewServer(identity.MiddlewareWithScopes(identity.Enforce, ids, nil, identity.ScopePolicy{}, api.NewHandler(api.Deps{Service: svc, SessionBootstrap: db})))
	t.Cleanup(srv.Close)
	return srv.URL[len("http://"):], operatorToken, agentToken, svc
}

// A launch through the MCP launch tool (daemon-routed, as `tether mcp` runs) and through the CLI's client both
// present the operator token, reach the API as the operator credential, and keep the task id; an agent's token on
// the same path is refused and creates nothing. This is the end-to-end proof that Torque or CLI launches do not
// silently lose the stamp (CW-20261012-0062a).
func TestTaskStampEndToEndOperatorAndAgent(t *testing.T) {
	hostport, operatorToken, agentToken, svc := daemonWithOperator(t)
	const id = "CW-20261016-0155"

	// MCP tether_session_create with the operator credential.
	op := NewWithDaemon(&app.Service{}, client.New("tcp:"+hostport, client.WithToken(operatorToken)), operatorToken, []string{ScopeSessionWrite})
	if _, err := op.handleSessionCreate(context.Background(), map[string]any{"launch_id": "l1", "task_id": id}); err != nil {
		t.Fatalf("operator MCP launch: %v", err)
	}
	if len(svc.inputs) != 1 || svc.inputs[0].TaskID != id || svc.inputs[0].LaunchID != "l1" {
		t.Fatalf("MCP launch lost the stamp: %+v", svc.inputs)
	}

	// The CLI's client (LaunchWithInput is what `tether launch --torque-task` calls).
	cli := client.New("tcp:"+hostport, client.WithToken(operatorToken))
	created, err := cli.CreateSessionWithInput(context.Background(), api.LaunchRequest{Launch: "l1", TaskID: id})
	if err != nil || created.ID != "created-1" {
		t.Fatalf("CLI client launch: %+v err=%v", created, err)
	}
	if len(svc.inputs) != 2 || svc.inputs[1].TaskID != id {
		t.Fatalf("CLI launch lost the stamp: %+v", svc.inputs)
	}

	// An agent's credential on the MCP path is refused, not silently stamped.
	ag := NewWithDaemon(&app.Service{}, client.New("tcp:"+hostport, client.WithToken(agentToken)), agentToken, []string{ScopeSessionWrite})
	if _, err := ag.handleSessionCreate(context.Background(), map[string]any{"launch_id": "l1", "task_id": id}); err == nil || !strings.Contains(err.Error(), "task_id_not_permitted") {
		t.Fatalf("an agent token on the MCP tool: err = %v, want task_id_not_permitted", err)
	}
	if len(svc.inputs) != 2 {
		t.Fatalf("a refused launch reached the service: %+v", svc.inputs)
	}
}
