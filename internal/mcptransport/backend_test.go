package mcptransport

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type backendFixture struct {
	*issuedRig
	backend *Backend
	addr    string
}

func newBackendFixture(t *testing.T) *backendFixture { return newBackendFixtureWith(t, nil) }

func newBackendFixtureWith(t *testing.T, configure func(*HandlerConfig)) *backendFixture {
	t.Helper()
	g := newIssuedRigWithTransport(t, true)
	cfg := g.f.handler.cfg
	if configure != nil {
		configure(&cfg)
	}
	b, err := NewBackend(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	addr := "unix:" + filepath.Join(filepath.Dir(strings.TrimPrefix(g.f.addr, "unix:")), "backend.sock")
	listener, err := net.Listen("unix", strings.TrimPrefix(addr, "unix:"))
	if err != nil {
		t.Fatal(err)
	}
	ds := &daemon.Server{Config: daemon.Config{ListenAddr: addr, IdentityMode: identity.Observe}, Identity: g.f.ids, Issuer: g.issuer, MCPBackend: b}
	server := &http.Server{Handler: ds.Handler(), ConnContext: identity.ConnectionContext, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { b.Close(); _ = server.Close(); ds.CloseIdentityAudit() })
	return &backendFixture{issuedRig: g, backend: b, addr: addr}
}

func (g *backendFixture) request(t *testing.T, method, path, token, body, session string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, daemon.BaseURL(g.addr)+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	response, err := daemon.DialHTTPClient(g.addr).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func requireBackendStatus(t *testing.T, response *http.Response, want int) []byte {
	t.Helper()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("HTTP %d, want %d: %s", response.StatusCode, want, raw)
	}
	return raw
}

func (g *backendFixture) admission(t *testing.T, path, token, body string) AdmissionResponse {
	t.Helper()
	response := g.request(t, "POST", path, token, body, "")
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("admission is cacheable")
	}
	raw := requireBackendStatus(t, response, 200)
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), "credential_id") || strings.Contains(string(raw), "local_connection") {
		t.Fatal("admission exposed credential/provenance")
	}
	var out AdmissionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBackendAdmissionUsesCurrentAuthorityAndStrictSelectors(t *testing.T) {
	g := newBackendFixture(t)
	token := g.f.token(t, "service", []string{})
	before := g.admission(t, AdmissionPath, token, `{}`)
	if before.SchemaVersion != 1 || !before.Principal.Verified || before.Principal.ID != "service" || before.MachineAccess != launch.MachineAccessReadOnly || before.Fingerprint == "" || before.Policy.Servers == nil {
		t.Fatalf("unexpected authority: %+v", before)
	}
	g.f.catMu.Lock()
	g.f.cat.MCPServerEnabled["other"] = true
	g.f.cat.Agents = map[string]config.LaunchProfile{"machine": {MachineAuthority: true, MachineAccess: launch.MachineAccessReadWrite}}
	g.f.cat.Global.Identity.MCPGrants["service"] = config.PrincipalMCPGrant{Servers: []string{"other"}, Agent: "machine"}
	g.f.catMu.Unlock()
	after := g.admission(t, AdmissionPath, token, `{"profile":"readonly","discovery_mode":"search"}`)
	if after.Fingerprint == before.Fingerprint || after.MachineAccess != launch.MachineAccessReadWrite || !slices.Equal(after.Policy.Servers, []string{"other"}) || after.Profile.ID != "readonly" || after.Selection.Mode != mcpgateway.Search {
		t.Fatalf("authority did not refresh: %+v", after)
	}
	for _, body := range []string{`null`, `[]`, `{"profile":null}`, `{"profile":""}`, `{"profile":"full","profile":"readonly"}`, `{"scopes":["*"]}`, `{"remote":false}`, `{"session_id":"s-other"}`, `{"profile":"full"} {}`, `{"profile":3}`} {
		requireBackendStatus(t, g.request(t, "POST", AdmissionPath, token, body, ""), 400)
	}
	requireBackendStatus(t, g.request(t, "POST", AdmissionPath+"?profile=full", token, `{}`, ""), 400)
	requireBackendStatus(t, g.request(t, "POST", AdmissionPath, token, `{"profile":"unknown"}`, ""), 403)
	requireBackendStatus(t, g.request(t, "POST", AdmissionPath, "", `{}`, ""), 401)
	g.f.verifier.unavailable.Store(true)
	requireBackendStatus(t, g.request(t, "POST", AdmissionPath, token, `{}`, ""), 503)
	g.f.verifier.unavailable.Store(false)
	if err := g.f.ids.RevokeToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	requireBackendStatus(t, g.request(t, "POST", AdmissionPath, token, `{}`, ""), 401)
}

func TestBackendRemoteAdmissionCannotBecomeLocal(t *testing.T) {
	g := newBackendFixture(t)
	service := g.f.token(t, "service", nil)
	operator, err := g.f.ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{service, operator} {
		requireBackendStatus(t, g.request(t, "POST", RemoteAdmissionPath, token, `{}`, ""), 404)
		g.admission(t, AdmissionPath, token, `{}`) // positive local control
	}
	token, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	out := g.admission(t, RemoteAdmissionPath, token, `{}`)
	if out.Principal.Kind != "agent" || out.Policy.SessionID != "s-rot" {
		t.Fatalf("unbound remote authority: %+v", out)
	}
	// Even the local route cannot manufacture socket proof from HTTP claims.
	for _, path := range []string{AdmissionPath, RemoteAdmissionPath, NativePath, RemoteNativePath} {
		req := httptest.NewRequest("POST", "http://localhost"+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+operator)
		req.Header.Set("X-Tether-Remote", "false")
		req.Header.Set("X-Tether-Local-Connection", "true")
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		rec := httptest.NewRecorder()
		g.backend.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("forged local proof on %s: %d", path, rec.Code)
		}
	}
	if err := g.f.db.UpdateSessionState("s-rot", "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	requireBackendStatus(t, g.request(t, "POST", RemoteAdmissionPath, token, `{}`, ""), 401)
}

type backendBearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t backendBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}
func (g *backendFixture) connect(t *testing.T, path, token string) *mcpsdk.ClientSession {
	t.Helper()
	hc := daemon.DialHTTPClient(g.addr)
	hc.Transport = backendBearerTransport{base: hc.Transport, token: token}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "backend-test", Version: "1"}, nil).Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: daemon.BaseURL(g.addr) + path, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestBackendNativeFloorsPinsScopesAndNoAggregateRuntime(t *testing.T) {
	g := newBackendFixture(t)
	g.f.catMu.Lock()
	g.f.cat.MCPServerEnabled["other"] = true
	g.f.cat.Global.MCP.Profiles["mixed"] = mcpgateway.Profile{Pinned: []string{"other_tool"}, Servers: []string{"tether", "other"}}
	g.f.catMu.Unlock()
	if err := g.f.db.CreateSession(store.SessionRow{ID: "s-native", LogicalAgentID: "agent", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	p := mcpgateway.SessionPolicy{SessionID: "s-native", AgentID: "agent", Servers: []string{"other"}, ToolProfile: &mcpgateway.Profile{Tools: mcpgateway.ToolRules{Allow: []string{"tether_docs_*", "tether_agent_create"}}}, EditableAgents: []string{"allowed"}}.Seal()
	if err := g.f.db.SaveSessionMCPPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	token, _ := g.mint(t, authissuer.TetherAudience, "s-native", "tether.read", "catalog.write")
	out := g.admission(t, RemoteAdmissionPath, token, `{"profile":"mixed","discovery_mode":"search"}`)
	if len(out.AuthorityProfiles) != 1 || !slices.Equal(out.Policy.EditableAgents, []string{"allowed"}) {
		t.Fatalf("missing immutable floor: %+v", out)
	}
	cs := g.connect(t, RemoteNativePath+"?profile=mixed&discovery_mode=search", token)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "tether_docs_list") || slices.Contains(names, "tether_health") {
		t.Fatalf("native floor not applied: %v", names)
	}
	for _, name := range []string{"tether_tool_call", "tether_tool_list", "tether_tool_search", "tether_gateway_status", "tether_catalog_refresh", "tether_result_next", "other_tool"} {
		if slices.Contains(names, name) {
			t.Fatalf("non-native helper registered: %s", name)
		}
	}
	success, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_docs_list"})
	if err != nil || success.IsError {
		t.Fatalf("native positive control: %+v %v", success, err)
	}
	for _, name := range []string{"tether_health", "tether_tool_call", "tether_events_tool_calls"} {
		res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name})
		if err == nil && !res.IsError {
			t.Fatalf("excluded direct call succeeded: %s", name)
		}
	}
	refused, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_agent_create", Arguments: map[string]any{"id": "forbidden"}})
	if err != nil || !refused.IsError {
		t.Fatalf("editable-agent restriction lost: %+v %v", refused, err)
	}
	raw, _ := json.Marshal(refused)
	if !strings.Contains(string(raw), "editable") {
		t.Fatalf("wrong refusal: %s", raw)
	}
	if g.backend.local.runtime != nil || g.backend.remote.runtime != nil {
		t.Fatal("native route constructed aggregate runtime")
	}
	// Scope enforcement remains in native handlers even when profile permits it.
	narrow, _ := g.mint(t, authissuer.TetherAudience, "s-other", "tether.read")
	ns := g.connect(t, RemoteNativePath, narrow)
	history, err := ns.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_events_tool_calls"})
	if err != nil || history.IsError {
		t.Fatalf("durable native history unavailable: %+v %v", history, err)
	}
	refused, err = ns.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_agent_create", Arguments: map[string]any{"id": "anything"}})
	if err != nil || !refused.IsError {
		t.Fatalf("native scope restriction lost: %+v %v", refused, err)
	}
	raw, _ = json.Marshal(refused)
	if !strings.Contains(string(raw), "scope") {
		t.Fatalf("wrong scope refusal: %s", raw)
	}
}

const backendInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
const backendList = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`

func TestBackendNativeRefreshRevocationAndIngressBinding(t *testing.T) {
	g := newBackendFixture(t)
	first, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	second, jti := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	response := g.request(t, "POST", RemoteNativePath, first, backendInitialize, "")
	id := response.Header.Get("Mcp-Session-Id")
	requireBackendStatus(t, response, 200)
	if id == "" {
		t.Fatal("missing native SDK session")
	}
	requireBackendStatus(t, g.request(t, "POST", RemoteNativePath, second, backendList, id), 200)
	requireBackendStatus(t, g.request(t, "POST", NativePath, second, backendList, id), 404)
	g.backend.remote.mu.Lock()
	view := g.backend.remote.sessions[id]
	adopted := view.token
	g.backend.remote.mu.Unlock()
	if adopted != second || view.localConnection {
		t.Fatal("native refresh/provenance not retained")
	}
	g.f.verifier.unavailable.Store(true)
	requireBackendStatus(t, g.request(t, "POST", RemoteNativePath, second, backendList, id), 503)
	g.f.verifier.unavailable.Store(false)
	requireBackendStatus(t, g.request(t, "POST", RemoteNativePath, second, backendList, id), 200)
	// Exercise SDK recheck directly, with no outer HTTP admission to protect it.
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := view.view.Server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "direct-recheck", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	g.f.verifier.unavailable.Store(true)
	if _, err := cs.ListTools(context.Background(), nil); err == nil {
		t.Fatal("SDK method used stale admission")
	}
	g.f.verifier.unavailable.Store(false)
	if err := g.issuer.State.Deny(authissuer.DenyJTI, jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requireBackendStatus(t, g.request(t, "POST", RemoteNativePath, second, backendList, id), 401)
	select {
	case <-view.callCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not cancel native view")
	}
}

func TestBackendMonitorEndsNativeViewOnPolicyOrSessionChange(t *testing.T) {
	for _, cause := range []string{"policy", "session"} {
		t.Run(cause, func(t *testing.T) {
			g := newBackendFixture(t)
			token, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
			response := g.request(t, "POST", RemoteNativePath+"?profile=full", token, backendInitialize, "")
			id := response.Header.Get("Mcp-Session-Id")
			requireBackendStatus(t, response, 200)
			g.backend.remote.mu.Lock()
			view := g.backend.remote.sessions[id]
			g.backend.remote.mu.Unlock()
			if view == nil {
				t.Fatal("missing admitted view")
			}
			if cause == "policy" {
				g.f.catMu.Lock()
				g.f.cat.Global.MCP.Profiles["full"] = mcpgateway.Profile{ReadOnly: true}
				g.f.catMu.Unlock()
			} else if err := g.f.db.UpdateSessionState("s-rot", "completed", 0, nil); err != nil {
				t.Fatal(err)
			}
			// No next HTTP request: the open-view monitor must revoke independently.
			select {
			case <-view.callCtx.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("authority change left native view alive")
			}
		})
	}
}

func TestBackendAdmissionParticipatesInDrain(t *testing.T) {
	for _, cancelDrain := range []bool{false, true} {
		name := "finish"
		if cancelDrain {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			g := newBackendFixtureWith(t, func(cfg *HandlerConfig) {
				load := cfg.Resolver.Catalog
				cfg.Resolver.Catalog = func(ctx context.Context) (*config.Catalog, error) {
					close(entered)
					select {
					case <-release:
						return load(ctx)
					case <-ctx.Done():
						close(canceled)
						return nil, ctx.Err()
					}
				}
			})
			token := g.f.token(t, "draining", nil)
			done := make(chan *http.Response, 1)
			go func() { done <- g.request(t, "POST", AdmissionPath, token, `{}`, "") }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("resolver never entered")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			drained := make(chan struct{})
			go func() { g.backend.Drain(ctx); close(drained) }()
			deadline := time.Now().Add(time.Second)
			for {
				g.backend.local.mu.Lock()
				closed := g.backend.local.closed
				g.backend.local.mu.Unlock()
				if closed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("drain never closed admission")
				}
				time.Sleep(time.Millisecond)
			}
			requireBackendStatus(t, g.request(t, "POST", AdmissionPath, token, `{}`, ""), 503)
			select {
			case <-drained:
				t.Fatal("drain returned while admission still reads state")
			default:
			}
			status := 200
			if cancelDrain {
				status = 503
				cancel()
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("drain cancellation did not reach resolver")
				}
			} else {
				close(release)
			}
			select {
			case response := <-done:
				requireBackendStatus(t, response, status)
			case <-time.After(time.Second):
				t.Fatal("admission did not finish")
			}
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("drain did not finish")
			}
		})
	}
}
