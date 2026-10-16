package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/libs/security/secrets/tokenfile"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// CW-20261016-0160, nothing seeded by hand: a real worker launch through the hub's service saves the session's MCP
// policy; the outpost's token minter gets the agent token from the hub's issuer; the worker's own mcp-http-relay
// then talks to the hub's REMOTE listener, whose gate hands the bound agent token to the daemon's real MCP gateway,
// built the way the daemon builds it. A refreshed token continues the session.
func TestWorkerLaunchThroughRelayToRemoteMCPEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := os.MkdirTemp("/var/tmp", "worker-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	catalog, repo := filepath.Join(root, "catalog"), filepath.Join(root, "repo")
	for path, body := range map[string]string{
		"global.yaml": fmt.Sprintf("version: 0.1.0\ncatalog:\n  defaults:\n    state_db: %q\ndaemon:\n  listen_addr: %q\n  pid_file: %q\n  shutdown_timeout: 2s\n  mcp_endpoint:\n    enabled: true\nidentity:\n  mode: enforce\nmcp:\n  discovery_mode: flat\n",
			filepath.Join(root, "state.db"), "unix:"+filepath.Join(root, "d.sock"), filepath.Join(root, "d.pid")),
		"projects/p.yaml":    "id: p\nname: P\nrepo_root: " + repo + "\nworkspace:\n  default_mode: shared\n  session_root: " + filepath.Join(root, "sessions") + "\n",
		"providers/cli.yaml": "id: claude\ntype: cli\ncommand: claude\n",
		"agents/worker.yaml": "id: worker\nname: Worker\neditable_agents:\n  - chat\n",
		"agents/chat.yaml":   "id: chat\nname: Chat\n",
		"launches/l1.yaml":   "id: l1\nproject: p\nagent: worker\nprovider: claude\nmcp:\n  servers: []\n",
	} {
		full := filepath.Join(catalog, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	svc, err := app.New(catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	cat, err := config.Load(catalog)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ids := identity.NewStore(svc.Store.DB())
	issuer, err := buildTokenIssuer(ctx, svc, filepath.Join(root, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := buildDaemonMCP(ctx, svc, cfg, ids, issuer, nil)
	if err != nil || mcp == nil {
		t.Fatalf("daemon MCP gateway: %v %v", mcp, err)
	}
	t.Cleanup(mcp.Close)
	server := &daemon.Server{Config: cfg, Identity: ids, Issuer: issuer, Workers: svc.Workers, Service: &serviceAdapter{svc: svc}, MCP: mcp}
	t.Cleanup(server.CloseIdentityAudit)

	// The hub's remote listener: loopback behind a reverse proxy, so the Host header is the public name, never the
	// loopback address (the first canary 403 came from the MCP SDK's own localhost guard on exactly that pairing).
	const publicHost = "tether.example"
	ts := httptest.NewUnstartedServer(nil)
	server.Config.RemoteListener = daemon.RemoteListenerConfig{Enabled: true, ListenAddr: "tcp:127.0.0.1:7331", AllowedHosts: []string{ts.Listener.Addr().String(), publicHost}}
	ts.Config.Handler = server.RemoteHandler(ts.Listener.Addr().String())
	ts.Config.ConnContext = identity.ConnectionContext
	ts.Start()
	t.Cleanup(ts.Close)

	// An enrolled worker device connects over the worker channel and announces what it can do.
	expires := time.Now().Add(time.Hour)
	deviceToken, err := ids.Mint(ctx, identity.Principal{ID: "msg://device/fake-worker", Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+deviceToken)
	header.Set("Tether-Protocol", "1")
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+workerwire.ConnectPath, header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	hello := workerwire.Frame{T: "hello", Proto: 1, Features: []string{"bind", "deliver", workerwire.FeatureSessionCred, workerwire.FeatureAgentToken, workerwire.FeatureEditableAgents}, Worker: &workerwire.Worker{Label: "fake-worker", Platform: "linux", Arch: "amd64"}}
	if err := ws.WriteJSON(hello); err != nil {
		t.Fatal(err)
	}
	var welcome workerwire.Frame
	if err := ws.ReadJSON(&welcome); err != nil || welcome.T != "welcome" {
		t.Fatalf("welcome: %+v %v", welcome, err)
	}
	specs := make(chan workerwire.LaunchSpec, 4)
	go func() {
		for {
			var f workerwire.Frame
			if ws.ReadJSON(&f) != nil {
				return
			}
			var spec workerwire.LaunchSpec
			_ = json.Unmarshal(f.Spec, &spec)
			specs <- spec
			_ = ws.WriteJSON(workerwire.Frame{T: "launched", CmdID: f.CmdID, Session: f.Session, PID: 7})
		}
	}()

	// The hub places and launches a session on that worker.
	created, err := svc.CreateSessionWithInput(app.CreateSessionInput{LaunchID: "l1", Worker: "fake-worker"})
	if err != nil {
		t.Fatalf("create worker session: %v", err)
	}
	// The catalog is enforce, so a launch always has a verified launcher; an
	// anonymous one would (rightly) be refused before any restart.
	operator := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if _, err := svc.LaunchSessionWithContext(operator, created.SessionID); err != nil {
		t.Fatalf("launch worker session: %v", err)
	}
	var spec workerwire.LaunchSpec
	select {
	case spec = <-specs:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never received the launch")
	}
	if !spec.MCPPolicy || len(spec.AgentTokens) != 1 || spec.AgentTokens[0].Aud != "tether" {
		t.Fatalf("launch spec = mcp_policy %v agent tokens %+v", spec.MCPPolicy, spec.AgentTokens)
	}
	policy, err := svc.Store.SessionMCPPolicy(ctx, created.SessionID)
	if err != nil || len(policy.EditableAgents) != 1 || policy.EditableAgents[0] != "chat" {
		t.Fatalf("saved worker policy = %+v err=%v", policy, err)
	}

	// The outpost mints the session's agent token from the hub and keeps it in a 0600 file.
	deviceFile := filepath.Join(root, "device.token")
	if err := tokenfile.Write(deviceFile, deviceToken); err != nil {
		t.Fatal(err)
	}
	minter := &workerruntime.TokenMinter{Hub: ts.URL, DeviceTokenFile: deviceFile}
	mint := func() string {
		tok, err := minter.Mint(ctx, "tether", spec.AgentTokens[0].Scopes, created.SessionID)
		if err != nil {
			t.Fatalf("mint agent token: %v", err)
		}
		return tok.Token
	}
	tokenPath := filepath.Join(root, "tether.token")
	if err := tokenfile.Write(tokenPath, mint()); err != nil {
		t.Fatal(err)
	}

	// The worker's relay against the hub's remote /mcp.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	relay := &workerruntime.HTTPRelay{URL: ts.URL + "/mcp", TokenFile: tokenPath, Stderr: io.Discard,
		Client: &http.Client{Transport: publicHostTransport{host: publicHost}}}
	done := make(chan error, 1)
	go func() { done <- relay.Run(ctx, inR, outW); _ = outW.Close() }()
	lines := bufio.NewScanner(outR)
	lines.Buffer(make([]byte, 0, 1<<20), 8<<20)
	call := func(body string) map[string]any {
		t.Helper()
		if _, err := io.WriteString(inW, body+"\n"); err != nil {
			t.Fatal(err)
		}
		got := make(chan map[string]any, 1)
		go func() {
			for lines.Scan() {
				var m map[string]any
				if json.Unmarshal(lines.Bytes(), &m) == nil && m["id"] != nil {
					got <- m
					return
				}
			}
			got <- nil
		}()
		select {
		case m := <-got:
			if m == nil {
				t.Fatal("relay output ended")
			}
			return m
		case <-time.After(15 * time.Second):
			t.Fatalf("no reply to %s", body)
			return nil
		}
	}
	init := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}`)
	if init["error"] != nil || init["result"] == nil {
		t.Fatalf("initialize through the remote /mcp: %v (gate denials %v)", init, server.RemoteMCPDenials())
	}
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	tools := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	result, _ := tools["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	if tools["error"] != nil || len(list) == 0 {
		t.Fatalf("tools/list through the remote /mcp: %v", tools)
	}
	// One tool call that goes through the gateway's native client to the daemon, authenticated with the bound token.
	names := map[string]bool{}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			names[fmt.Sprint(m["name"])] = true
		}
	}
	if !names["tether_health"] {
		t.Fatalf("tools/list lacks tether_health: %v", names)
	}
	callResult := call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tether_health","arguments":{}}}`)
	res, _ := callResult["result"].(map[string]any)
	if callResult["error"] != nil || res == nil || res["isError"] == true {
		t.Fatalf("tools/call tether_health through the remote /mcp: %v", callResult)
	}
	// The outpost refreshes the token in place; the relay re-reads it and the session continues.
	if err := tokenfile.Write(tokenPath, mint()); err != nil {
		t.Fatal(err)
	}
	again := call(`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	if again["error"] != nil || again["result"] == nil {
		t.Fatalf("tools/list after the token was refreshed: %v", again)
	}
	_ = inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not stop")
	}
	// The remote initialize was audited.
	server.CloseIdentityAudit()
	var n int
	if err := svc.Store.DB().QueryRow(`SELECT COUNT(*) FROM identity_audit WHERE event LIKE 'remote_mcp_initialize worker=%' AND session_id = ?`, created.SessionID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("remote initialize audit rows = %d err=%v", n, err)
	}
}

// publicHostTransport sends requests as the reverse proxy would: connection to the loopback listener, Host header the
// public name.
type publicHostTransport struct{ host string }

func (t publicHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = t.host
	return http.DefaultTransport.RoundTrip(r)
}
