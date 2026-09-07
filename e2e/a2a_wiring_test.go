package e2e

// a2a_wiring_test.go — T11 "A2A/bridge auth" scenario, the real-process
// half. T10's own internal/a2aadapter tests already thoroughly prove the
// adapter's own protocol/auth correctness via a real a2a-go SDK client
// against a real Adapter — but always in-process (httptest.NewServer),
// never through the actual <catalogRoot>/a2a/*.yaml catalog loader
// (internal/config.LoadA2ABindings) feeding a genuinely running `mux
// daemon run` process. This test proves that specific wiring path end to
// end: write a real binding YAML file, boot the real daemon against it,
// and drive it with a real a2a-go client (over the daemon's real UDS
// socket, via daemon.DialHTTPClient — the same UDS-transport trick every
// other Tether client uses) to confirm the configured binding is
// reachable and its bearer-token auth is enforced for real.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hollis-labs/tether/internal/daemon"
)

// bearerInjectingTransport wraps a base RoundTripper, setting a fixed
// Authorization header on every request (or none, if token is empty) --
// simulates a real external A2A caller either presenting or omitting
// credentials.
type bearerInjectingTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerInjectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	if t.token != "" {
		req2.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req2)
}

func udsHTTPClient(socketAddr, bearerToken string) *http.Client {
	base := daemon.DialHTTPClient(socketAddr)
	return &http.Client{
		Transport: &bearerInjectingTransport{base: base.Transport, token: bearerToken},
		Timeout:   base.Timeout,
	}
}

func TestA2AWiring_RealDaemonServesConfiguredBindingAndEnforcesAuth(t *testing.T) {
	stateRoot, err := os.MkdirTemp("", "te2ea2a")
	if err != nil {
		t.Fatalf("mkdir state root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })

	a2aDir := filepath.Join(stateRoot, "catalog", "a2a")
	if err := os.MkdirAll(a2aDir, 0o750); err != nil {
		t.Fatalf("mkdir catalog/a2a: %v", err)
	}
	bindingYAML := `
id: greeter
target_urn: "msg://agent/agent-mux/agt_e2agreeter00"
display_name: "E2E Greeter"
base_url: "http://unix/a2a"
bearer_token: "e2e-secret-token"
task_mode: false
`
	if err := os.WriteFile(filepath.Join(a2aDir, "greeter.yaml"), []byte(bindingYAML), 0o640); err != nil {
		t.Fatalf("write binding yaml: %v", err)
	}

	d := startFixtureDaemonAt(t, stateRoot)
	base := daemon.BaseURL(d.SocketAddr)

	// Discovery is unauthenticated by design (T10) -- confirm it works
	// through the real daemon process regardless.
	resolver := agentcard.Resolver{Client: udsHTTPClient(d.SocketAddr, "")}
	card, err := resolver.Resolve(t.Context(), base+"/a2a/agents/greeter"+a2asrv.WellKnownAgentCardPath)
	if err != nil {
		t.Fatalf("resolve agent card via real daemon: %v", err)
	}
	if card.Name != "E2E Greeter" {
		t.Fatalf("card name = %q, want %q (proves the real catalog YAML actually fed the running adapter)", card.Name, "E2E Greeter")
	}

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello over a real socket"))

	noAuthClient, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithJSONRPCTransport(udsHTTPClient(d.SocketAddr, "")))
	if err != nil {
		t.Fatalf("NewFromCard (no auth): %v", err)
	}
	if _, err := noAuthClient.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg}); err == nil {
		t.Fatal("expected an error sending with no bearer token against a secured binding")
	} else if !strings.Contains(err.Error(), "bearer token") {
		t.Fatalf("no-auth error = %v, want an auth-shaped rejection (not e.g. a 404 masking a routing bug)", err)
	}

	authedClient, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithJSONRPCTransport(udsHTTPClient(d.SocketAddr, "e2e-secret-token")))
	if err != nil {
		t.Fatalf("NewFromCard (authed): %v", err)
	}
	result, err := authedClient.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello, authenticated"))})
	if err != nil {
		t.Fatalf("SendMessage with correct bearer token over the real daemon: %v", err)
	}
	if _, ok := result.(*a2a.Message); !ok {
		t.Fatalf("result = %T, want *a2a.Message (task_mode: false)", result)
	}
}
