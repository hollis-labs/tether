package mcptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpforward"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type forwardedContextKey struct{}

type transportFixture struct {
	bus         events.Bus
	db          *store.Store
	ids         *identity.Store
	verifier    *switchableVerifier
	handler     *Handler
	addr        string
	cat         *config.Catalog
	catMu       sync.Mutex
	childStarts string
	closeHTTP   func() error
}

func newTransportFixture(t *testing.T, unix, upstream bool) *transportFixture {
	t.Helper()
	return newTransportFixtureWithTimeout(t, unix, upstream, time.Minute)
}

func newTransportFixtureWithTimeout(t *testing.T, unix, upstream bool, timeout time.Duration) *transportFixture {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home, catalog, run, state := filepath.Join(root, "home"), filepath.Join(root, "catalog"), filepath.Join(root, "run"), filepath.Join(root, "state")
	for _, dir := range []string{home, catalog, run, state} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("TETHER_TOKEN", "")
	db, err := store.Open(filepath.Join(state, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &transportFixture{db: db, ids: identity.NewStore(db.DB()), cat: &config.Catalog{MCPServerEnabled: map[string]bool{}}, childStarts: filepath.Join(root, "child-starts")}
	f.cat.Global.MCP.Profiles = map[string]mcpgateway.Profile{"readonly": {ReadOnly: true}, "full": {}}
	entries := []config.MCPServerEntry{}
	if upstream {
		if output, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "--unshare-pid", "--proc", "/proc", "--dev", "/dev", "--", "true").CombinedOutput(); err != nil {
			_ = db.Close()
			t.Skipf("bubblewrap unavailable: %v %s", err, output)
		}
		f.cat.MCPServerEnabled["app"] = true
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, config.MCPServerEntry{ID: "app", Transport: "stdio", Command: executable, Args: []string{"-test.run=^TestTransportUpstreamProcess$"}, Env: map[string]string{"TETHER_TRANSPORT_FIXTURE": f.childStarts}})
	}
	f.verifier = &switchableVerifier{inner: f.ids}
	var listener net.Listener
	if unix {
		f.addr = "unix:" + filepath.Join(run, "test.sock")
		listener, err = net.Listen("unix", strings.TrimPrefix(f.addr, "unix:"))
	} else {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			f.addr = "tcp:" + listener.Addr().String()
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	f.bus = events.NewBus(events.BusOptions{Persister: db})
	recordCtx, stopRecorder := context.WithCancel(context.Background())
	svc := &app.Service{Store: db, Catalog: f.cat, CatalogRoot: catalog, Bus: f.bus}
	h, err := NewHandler(context.Background(), HandlerConfig{
		ListenAddr: f.addr, IdentityMode: identity.Observe, Verifier: f.verifier, Service: svc, Publisher: mcpadapter.NewDaemonToolCallRecorder(recordCtx, f.bus, db), RecheckInterval: 20 * time.Millisecond, SessionTimeout: timeout, MaxViews: 16,
		NativeClient: func(token string) *client.Client { return client.New(f.addr, client.WithToken(token)) },
		Resolver: CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) {
			f.catMu.Lock()
			defer f.catMu.Unlock()
			raw, _ := json.Marshal(f.cat)
			var cat config.Catalog
			_ = json.Unmarshal(raw, &cat)
			return &cat, nil
		}, Session: db.SessionMCPPolicy},
		NewRuntime: func(_ context.Context, cat *config.Catalog) (*mcpadapter.SharedUpstreams, error) {
			return mcpadapter.NewSharedUpstreams(entries, mcpadapter.DaemonProtectedRoots{Catalog: catalog, Run: run, State: state, CatalogConfig: cat}, true)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	daemonServer := &daemon.Server{Config: daemon.Config{ListenAddr: f.addr, IdentityMode: identity.Observe}, Identity: f.ids, MCP: h}
	server := &http.Server{Handler: daemonServer.Handler(), ReadHeaderTimeout: time.Second}
	f.closeHTTP = server.Close
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		stopRecorder()
		h.Close()
		_ = server.Close()
		daemonServer.CloseIdentityAudit()
		_ = db.Close()
	})
	return f
}

func (f *transportFixture) token(t *testing.T, id string, servers []string) string {
	t.Helper()
	f.catMu.Lock()
	if f.cat.Global.Identity.MCPGrants == nil {
		f.cat.Global.Identity.MCPGrants = map[string]config.PrincipalMCPGrant{}
	}
	f.cat.Global.Identity.MCPGrants[id] = config.PrincipalMCPGrant{Servers: servers}
	f.catMu.Unlock()
	token, err := f.ids.Mint(context.Background(), identity.Principal{ID: id, Kind: "service"})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *transportFixture) request(t *testing.T, method, path, token, session string, headers map[string][]string) *http.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	if session != "" {
		body = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	}
	req, err := http.NewRequest(method, daemon.BaseURL(f.addr)+path, strings.NewReader(body))
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
	for key, values := range headers {
		if key == "Host" {
			req.Host = values[0]
		} else {
			req.Header[key] = values
		}
	}
	response, err := daemon.DialHTTPClient(f.addr).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func closeResponse(response *http.Response) { _ = response.Body.Close() }

func TestTransportAdmissionSelectorsAndViewBinding(t *testing.T) {
	for _, unix := range []bool{false, true} {
		t.Run(fmt.Sprintf("unix=%v", unix), func(t *testing.T) {
			f := newTransportFixture(t, unix, false)
			alpha, beta := f.token(t, "alpha", nil), f.token(t, "beta", nil)
			for _, tc := range []struct {
				name, path, token string
				headers           map[string][]string
				status            int
			}{
				{"missing", "/mcp", "", nil, 401}, {"invalid", "/mcp", "tth_invalid", nil, 401},
				{"bad-profile", "/p/missing", alpha, nil, 403}, {"empty-profile", "/mcp?profile=", alpha, nil, 403},
				{"profile-conflict", "/p/readonly?profile=full", alpha, nil, 403}, {"mode-conflict", "/mcp?discovery_mode=flat", alpha, map[string][]string{"X-Tether-Discovery-Mode": {"search"}}, 403},
				{"query-token", "/mcp?access_token=canary", alpha, nil, 400}, {"foreign-origin", "/mcp", alpha, map[string][]string{"Origin": {"http://evil.example"}}, 403},
				{"null-origin", "/mcp", alpha, map[string][]string{"Origin": {"null"}}, 403}, {"duplicate-origin", "/mcp", alpha, map[string][]string{"Origin": {"http://localhost", "http://localhost"}}, 403},
				{"foreign-host", "/mcp", alpha, map[string][]string{"Host": {"evil.example"}}, 403}, {"duplicate-auth", "/mcp", alpha, map[string][]string{"Authorization": {"Bearer " + alpha, "Bearer " + beta}}, 401},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := f.request(t, http.MethodPost, tc.path, tc.token, "", tc.headers)
					defer closeResponse(r)
					if r.StatusCode != tc.status {
						data, _ := io.ReadAll(r.Body)
						t.Fatalf("status=%d want=%d: %s", r.StatusCode, tc.status, data)
					}
				})
			}
			for _, path := range []string{"/mcp?profile=readonly", "/p/readonly", "/mcp"} {
				r := f.request(t, http.MethodPost, path, alpha, "", nil)
				if r.StatusCode != 200 {
					data, _ := io.ReadAll(r.Body)
					t.Fatalf("initialize: %d %s", r.StatusCode, data)
				}
				session := r.Header.Get("Mcp-Session-Id")
				closeResponse(r)
				if session == "" {
					t.Fatal("missing bound session")
				}
				r = f.request(t, http.MethodPost, path, beta, session, nil)
				closeResponse(r)
				if r.StatusCode != 403 {
					t.Fatal("cross-principal session accepted", r.StatusCode)
				}
				separator := "?"
				if strings.Contains(path, "?") {
					separator = "&"
				}
				r = f.request(t, http.MethodPost, path+separator+"discovery_mode=search", alpha, session, nil)
				closeResponse(r)
				if r.StatusCode == 200 {
					t.Fatal("session mode changed without initialization")
				}
				r = f.request(t, http.MethodDelete, path, alpha, session, nil)
				closeResponse(r)
				if r.StatusCode != http.StatusNoContent {
					t.Fatal("owner DELETE refused", r.StatusCode)
				}
			}
			if unix {
				r := f.request(t, http.MethodPost, "/mcp", alpha, "", map[string][]string{"Host": {"localhost"}, "Origin": {"http://localhost"}})
				defer closeResponse(r)
				if r.StatusCode != 200 {
					t.Fatal("UDS localhost refused", r.StatusCode)
				}
			}
		})
	}
}

func TestTransportRevocationClosesOpenStream(t *testing.T) {
	f := newTransportFixture(t, true, false)
	token := f.token(t, "alpha", nil)
	r := f.request(t, http.MethodPost, "/mcp", token, "", nil)
	session := r.Header.Get("Mcp-Session-Id")
	closeResponse(r)
	stream := f.request(t, http.MethodGet, "/mcp", token, session, nil)
	if stream.StatusCode != 200 {
		closeResponse(stream)
		t.Fatal("GET stream failed", stream.StatusCode)
	}
	defer closeResponse(stream)
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, stream.Body); done <- err }()
	if _, err := f.db.DB().Exec("UPDATE principals SET revoked_at=? WHERE token_hash=?", time.Now().UTC().Format(time.RFC3339Nano), identity.HashToken(token)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked token left stream open")
	}
	r = f.request(t, http.MethodPost, "/mcp", token, session, nil)
	defer closeResponse(r)
	if r.StatusCode != 401 {
		t.Fatal("revoked token reused", r.StatusCode)
	}
}

func TestTransportSessionPolicyFloorAndTerminalState(t *testing.T) {
	f := newTransportFixture(t, true, false)
	if err := f.db.CreateSession(store.SessionRow{ID: "actual", LogicalAgentID: "agent", State: "created"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	profile := "readonly"
	floor := mcpgateway.Profile{ReadOnly: true}
	if err := f.db.SaveSessionMCPPolicy(context.Background(), mcpgateway.SessionPolicy{SessionID: "actual", AgentID: "agent", Servers: []string{}, Profile: &profile, LaunchProfile: &floor}.Seal()); err != nil {
		t.Fatal(err)
	}
	token, err := f.ids.Mint(context.Background(), identity.Principal{ID: "session:actual", Kind: "session", SessionID: "actual"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.New(f.addr, client.WithToken(token)).ConnectMCP(context.Background(), client.MCPOptions{Profile: strPointer("full")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatal("requested full loosened captured readonly", tool.Name)
		}
	}
	if err := f.db.UpdateSessionState("actual", "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ListTools(context.Background(), nil); err == nil {
		t.Fatal("terminal credential kept tools")
	}
}

func strPointer(value string) *string { return &value }

func TestTransportCredentialSubstitutionAndPolicyChange(t *testing.T) {
	f := newTransportFixture(t, true, false)
	first := f.token(t, "same-principal", nil)
	second, err := f.ids.Mint(context.Background(), identity.Principal{ID: "same-principal", Kind: "service"})
	if err != nil {
		t.Fatal(err)
	}
	r := f.request(t, http.MethodPost, "/p/readonly", first, "", nil)
	id := r.Header.Get("Mcp-Session-Id")
	closeResponse(r)
	r = f.request(t, http.MethodPost, "/p/readonly", second, id, nil)
	closeResponse(r)
	if r.StatusCode != 403 {
		t.Fatal("another credential hijacked same-principal session", r.StatusCode)
	}
	f.catMu.Lock()
	f.cat.Global.MCP.Profiles["readonly"] = mcpgateway.Profile{}
	f.catMu.Unlock()
	r = f.request(t, http.MethodPost, "/p/readonly", first, id, nil)
	closeResponse(r)
	if r.StatusCode == 200 {
		t.Fatal("profile change widened existing view")
	}
	// A current grant change also invalidates a prior policy binding.
	r = f.request(t, http.MethodPost, "/mcp", first, "", nil)
	id = r.Header.Get("Mcp-Session-Id")
	closeResponse(r)
	f.catMu.Lock()
	f.cat.MCPServerEnabled["new"] = true
	f.cat.Global.Identity.MCPGrants["same-principal"] = config.PrincipalMCPGrant{Servers: []string{"new"}}
	f.catMu.Unlock()
	r = f.request(t, http.MethodPost, "/mcp", first, id, nil)
	closeResponse(r)
	if r.StatusCode == 200 {
		t.Fatal("catalog grant widened initialized view")
	}
}

type unavailableVerifier struct{}

func (unavailableVerifier) Verify(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, errors.New("storage unavailable CANARY_SECRET")
}

func TestTransportVerifierUnavailableAndOff(t *testing.T) {
	for _, mode := range []identity.Mode{identity.Observe, identity.Off} {
		h, err := NewHandler(context.Background(), HandlerConfig{ListenAddr: "unix:/unused.sock", IdentityMode: mode, Verifier: unavailableVerifier{}, Resolver: CallerResolver{Catalog: func(context.Context) (*config.Catalog, error) { t.Fatal("unverified catalog lookup"); return nil, nil }}, NewRuntime: func(context.Context, *config.Catalog) (*mcpadapter.SharedUpstreams, error) {
			t.Fatal("unverified runtime construction")
			return nil, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPost, "http://unix/mcp", nil)
		request.Header.Set("Authorization", "Bearer tth_invalid")
		writer := httptest.NewRecorder()
		h.ServeHTTP(writer, request)
		h.Close()
		if writer.Code != 503 || strings.Contains(writer.Body.String(), "CANARY_SECRET") {
			t.Fatalf("verifier failure leaked or failed open: %d %s", writer.Code, writer.Body.String())
		}
	}
	if _, err := NewHandler(context.Background(), HandlerConfig{ListenAddr: "tcp:192.0.2.1:8888", IdentityMode: identity.Enforce, Verifier: unavailableVerifier{}}); err == nil {
		t.Fatal("non-loopback enforce started with broken verifier")
	}
}

func TestTransportConcurrentClientsShareConfinedChild(t *testing.T) {
	var wireSurface []string
	for _, unix := range []bool{false, true} {
		t.Run(fmt.Sprintf("unix=%v", unix), func(t *testing.T) {
			f := newTransportFixture(t, unix, true)
			if _, err := os.Stat(f.childStarts); !os.IsNotExist(err) {
				t.Fatal("child started before admission")
			}
			zero := f.token(t, "zero", nil)
			cs, err := client.New(f.addr, client.WithToken(zero)).ConnectMCP(context.Background(), client.MCPOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for tool, err := range cs.Tools(context.Background(), nil) {
				if err != nil {
					t.Fatal(err)
				}
				if tool.Name == "app_echo" {
					t.Fatal("zero grant exposed upstream")
				}
			}
			_ = cs.Close()
			if _, err := os.Stat(f.childStarts); !os.IsNotExist(err) {
				t.Fatal("zero grant started upstream")
			}
			a, b := f.token(t, "alpha", []string{"app"}), f.token(t, "beta", []string{"app"})
			listed, err := client.New(f.addr, client.WithToken(a)).ConnectMCP(context.Background(), client.MCPOptions{})
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for tool, err := range listed.Tools(context.Background(), nil) {
				if err != nil {
					t.Fatal(err)
				}
				names = append(names, tool.Name)
			}
			_ = listed.Close()
			slices.Sort(names)
			if wireSurface == nil {
				wireSurface = names
			} else if !slices.Equal(wireSurface, names) {
				t.Fatal("HTTP/UDS surfaces differ")
			}
			if !slices.Contains(names, "app_echo") || !slices.Contains(names, "tether_health") {
				t.Fatal("gateway/native tools missing", names)
			}

			var wg sync.WaitGroup
			wg.Add(2)
			results := make(chan int, 2)
			for _, token := range []string{a, b} {
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					s, err := client.New(f.addr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = s.Close() }()
					reply, err := s.CallTool(ctx, &mcpsdk.CallToolParams{Name: "app_echo", Arguments: map[string]any{"message": "hello"}})
					if err != nil || reply.IsError {
						t.Errorf("call: %+v %v", reply, err)
						return
					}
					var output struct {
						PID int `json:"pid"`
					}
					for _, content := range reply.Content {
						if text, ok := content.(*mcpsdk.TextContent); ok {
							_ = json.Unmarshal([]byte(text.Text), &output)
						}
					}
					results <- output.PID
				}()
			}
			wg.Wait()
			close(results)
			pids := []int{}
			for pid := range results {
				pids = append(pids, pid)
			}
			if len(pids) != 2 || pids[0] == 0 || pids[0] != pids[1] {
				t.Fatal("clients did not share child", pids)
			}
			starts, err := os.ReadFile(f.childStarts)
			if err != nil || strings.Count(string(starts), "started\n") != 1 {
				t.Fatalf("upstream launched more than once: %q %v", starts, err)
			}
		})
	}
}

func TestTransportUpstreamProcess(t *testing.T) {
	path := os.Getenv("TETHER_TRANSPORT_FIXTURE")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = file.WriteString("started\n")
	_ = file.Close()
	s := gomcp.NewServer("transport-fixture", "1")
	s.SDKServer().AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method == "tools/call" {
				ctx = context.WithValue(ctx, forwardedContextKey{}, req.GetParams().GetMeta()["tether.context"])
			}
			return next(ctx, method, req)
		}
	})
	s.RegisterTool(gomcp.Tool{Name: "app_echo", Description: "Read-only echo", InputSchema: gomcp.InputSchema(gomcp.StringProp("message", "message", false)), ReadOnlyHint: true, Handler: func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"pid": os.Getpid(), "message": args["message"], "context": ctx.Value(forwardedContextKey{})}, nil
	}})
	s.RegisterTool(gomcp.Tool{Name: "app_slow", Description: "Slow fixture", InputSchema: gomcp.InputSchema(), ReadOnlyHint: true, Handler: func(ctx context.Context, _ map[string]any) (any, error) {
		if err := os.WriteFile(path+".call", []byte("started"), 0600); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
			return "finished", nil
		}
	}})
	s.RegisterTool(gomcp.Tool{Name: "app_count", Description: "Side effect fixture", InputSchema: gomcp.InputSchema(), Handler: func(ctx context.Context, _ map[string]any) (any, error) {
		effects, err := os.OpenFile(path+".effects", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, err = effects.WriteString("effect\n")
		_ = effects.Close()
		if err != nil {
			return nil, err
		}
		for {
			if _, err := os.Stat(path + ".release"); err == nil {
				return "committed", nil
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}})
	if s.Run(context.Background()) != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestTransportRefusesNonLoopbackWithoutAuthentication(t *testing.T) {
	for _, tc := range []struct {
		addr     string
		mode     identity.Mode
		verifier identity.Verifier
	}{{"tcp:0.0.0.0:8888", identity.Observe, nil}, {"tcp:192.0.2.1:8888", identity.Off, nil}, {"tcp:0.0.0.0:8888", identity.Enforce, nil}} {
		if _, err := NewHandler(context.Background(), HandlerConfig{ListenAddr: tc.addr, IdentityMode: tc.mode, Verifier: tc.verifier}); err == nil {
			t.Fatal("unauthenticated non-loopback accepted", tc.addr)
		}
	}
}

func TestTransportDaemonDownIsTypedAndNoFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := client.New("unix:"+filepath.Join(t.TempDir(), "absent.sock"), client.WithToken("")).ConnectMCP(context.Background(), client.MCPOptions{})
	if !errors.Is(err, client.ErrDaemonUnreachable) {
		t.Fatalf("untyped daemon-down: %v", err)
	}
}

func TestTransportCloseCancelsRuntimePreparation(t *testing.T) {
	f := newTransportFixture(t, true, false)
	token := f.token(t, "alpha", nil)
	started := make(chan struct{})
	f.handler.cfg.NewRuntime = func(ctx context.Context, _ *config.Catalog) (*mcpadapter.SharedUpstreams, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan int, 1)
	go func() {
		response := f.request(t, http.MethodPost, "/mcp", token, "", nil)
		done <- response.StatusCode
		closeResponse(response)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runtime factory never started")
	}
	closed := make(chan struct{})
	go func() { f.handler.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close left daemon preparation alive")
	}
	select {
	case status := <-done:
		if status != 503 {
			t.Fatal("canceled preparation accepted", status)
		}
	case <-time.After(time.Second):
		t.Fatal("initialization hung after shutdown")
	}
}

func TestTransportViewCapacityRecoversAfterDelete(t *testing.T) {
	f := newTransportFixture(t, true, false)
	f.handler.cfg.MaxViews = 1
	token := f.token(t, "alpha", nil)
	r := f.request(t, http.MethodPost, "/mcp", token, "", nil)
	id := r.Header.Get("Mcp-Session-Id")
	closeResponse(r)
	r = f.request(t, http.MethodPost, "/mcp", token, "", nil)
	closeResponse(r)
	if r.StatusCode != 503 {
		t.Fatal("unbounded view admission", r.StatusCode)
	}
	r = f.request(t, http.MethodDelete, "/mcp", token, id, nil)
	closeResponse(r)
	deadline := time.Now().Add(time.Second)
	for {
		r = f.request(t, http.MethodPost, "/mcp", token, "", nil)
		closeResponse(r)
		if r.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleted view retained capacity", r.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Transient verification errors must not destroy SDK session identity.
type switchableVerifier struct {
	inner       identity.Verifier
	unavailable atomic.Bool
}

func (v *switchableVerifier) Verify(ctx context.Context, token string) (identity.Principal, error) {
	if v.unavailable.Load() {
		return identity.Principal{}, errors.New("temporary verifier outage")
	}
	return v.inner.Verify(ctx, token)
}

func TestTransportTransientVerifierRetainsViews(t *testing.T) {
	f := newTransportFixture(t, true, false)
	token := f.token(t, "healthy", nil)
	response := f.request(t, http.MethodPost, "/mcp", token, "", nil)
	id := response.Header.Get("Mcp-Session-Id")
	closeResponse(response)
	f.verifier.unavailable.Store(true)
	time.Sleep(100 * time.Millisecond)
	response = f.request(t, http.MethodPost, "/mcp", token, id, nil)
	if response.StatusCode != 503 {
		t.Fatal(response.StatusCode)
	}
	closeResponse(response)
	f.verifier.unavailable.Store(false)
	response = f.request(t, http.MethodPost, "/mcp", token, id, nil)
	defer closeResponse(response)
	if response.StatusCode != 200 {
		t.Fatal("healthy view evicted", response.StatusCode)
	}
}

func TestTransportPrincipalQuotaPreservesOtherAndOperatorViews(t *testing.T) {
	f := newTransportFixture(t, true, false)
	a, b := f.token(t, "a", nil), f.token(t, "b", nil)
	// The fixture's smaller global cap exercises reserved operator headroom.
	for i := 0; i < 12; i++ {
		r := f.request(t, http.MethodPost, "/mcp", a, "", nil)
		if r.StatusCode != 200 {
			t.Fatal(i, r.StatusCode)
		}
		closeResponse(r)
	}
	r := f.request(t, http.MethodPost, "/mcp", b, "", nil)
	if r.StatusCode != 503 {
		t.Fatal("headroom consumed", r.StatusCode)
	}
	closeResponse(r)
	operator, err := f.ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	r = f.request(t, http.MethodPost, "/mcp", operator, "", nil)
	if r.StatusCode != 200 {
		t.Fatal("operator locked out", r.StatusCode)
	}
	closeResponse(r)
}

func TestTransportRevocationCancelsInflightCallAndDrainWaits(t *testing.T) {
	for _, revoke := range []bool{true, false} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			f := newTransportFixture(t, true, true)
			token := f.token(t, "slow", []string{"app"})
			session, err := client.New(f.addr, client.WithToken(token)).ConnectMCP(context.Background(), client.MCPOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close() }()
			done := make(chan error, 1)
			go func() {
				_, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "app_slow"})
				done <- err
			}()
			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(f.childStarts + ".call"); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("call never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			started := time.Now()
			if revoke {
				if _, err := f.db.DB().Exec(`UPDATE principals SET revoked_at=CURRENT_TIMESTAMP WHERE token_hash=?`, identity.HashToken(token)); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("revocation left call running")
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				f.handler.Drain(ctx)
				if elapsed := time.Since(started); elapsed < time.Second {
					t.Fatal("shutdown canceled call instead of draining", elapsed)
				}
				if err := <-done; err != nil {
					t.Fatal("drained call failed", err)
				}
			}
		})
	}
}

func TestTransportPerPrincipalLimitDoesNotLockOthersOut(t *testing.T) {
	f := newTransportFixture(t, true, false)
	f.handler.cfg.MaxViews = 128
	a, b := f.token(t, "abusive", nil), f.token(t, "other", nil)
	for i := 0; i < 16; i++ {
		r := f.request(t, http.MethodPost, "/mcp", a, "", nil)
		if r.StatusCode != 200 {
			t.Fatal(i, r.StatusCode)
		}
		closeResponse(r)
	}
	r := f.request(t, http.MethodPost, "/mcp", a, "", nil)
	if r.StatusCode != 503 {
		t.Fatal("principal limit missing", r.StatusCode)
	}
	closeResponse(r)
	r = f.request(t, http.MethodPost, "/mcp", b, "", nil)
	defer closeResponse(r)
	if r.StatusCode != 200 {
		t.Fatal("other principal locked out", r.StatusCode)
	}
}

func TestTransportManyHealthyViewsRetainSessionIDs(t *testing.T) {
	f := newTransportFixture(t, true, false)
	f.handler.cfg.MaxViews = 128
	type binding struct{ token, id string }
	bindings := []binding{}
	for principal := 0; principal < 7; principal++ {
		token := f.token(t, fmt.Sprintf("healthy-%d", principal), nil)
		for j := 0; j < 15; j++ {
			r := f.request(t, http.MethodPost, "/mcp", token, "", nil)
			if r.StatusCode != 200 {
				t.Fatal("initialize", len(bindings), r.StatusCode)
			}
			bindings = append(bindings, binding{token, r.Header.Get("Mcp-Session-Id")})
			closeResponse(r)
		}
	}
	time.Sleep(100 * time.Millisecond)
	for _, b := range bindings {
		r := f.request(t, http.MethodPost, "/mcp", b.token, b.id, nil)
		if r.StatusCode != 200 {
			t.Fatal("healthy session evicted", r.StatusCode)
		}
		closeResponse(r)
	}
}

func TestThinForwardersShareDaemonPoolOverUnix(t *testing.T) {
	f := newTransportFixture(t, true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probeToken := f.token(t, "probe", []string{"app"})
	if err := client.New(f.addr, client.WithToken(probeToken)).ProbeMCP(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.childStarts); !os.IsNotExist(err) {
		t.Fatal("doctor probe initialized an upstream", err)
	}
	for _, id := range []string{"alpha", "beta"} {
		if err := f.db.CreateSession(store.SessionRow{ID: id, LogicalAgentID: "agent-" + id, State: "running"}, &launch.Plan{}); err != nil {
			t.Fatal(err)
		}
		if err := f.db.SaveSessionMCPPolicy(ctx, mcpgateway.SessionPolicy{SessionID: id, AgentID: "agent-" + id, Servers: []string{"app"}}.Seal()); err != nil {
			t.Fatal(err)
		}
		token, err := f.ids.Mint(ctx, identity.Principal{ID: "session:" + id, Kind: "session", SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		stream, stop, err := f.bus.Subscribe(ctx, events.Filter{SessionID: id})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		left, right := mcpsdk.NewInMemoryTransports()
		done := make(chan error, 1)
		go func() {
			done <- mcpforward.Run(ctx, client.New(f.addr, client.WithToken(token)), client.MCPOptions{}, right)
		}()
		session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "thin-test", Version: "1"}, nil).Connect(ctx, left, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "app_echo", Arguments: map[string]any{"message": id}, Meta: mcpsdk.Meta{"Tether.Context": map[string]any{"session_id": "forged"}, "tether.provenance": map[string]any{"session_id": "forged"}}})
		if err != nil || result.IsError {
			t.Fatalf("forwarded call: %+v %v", result, err)
		}
		body, _ := json.Marshal(result)
		if !strings.Contains(string(body), id) {
			t.Fatalf("caller result crossed: %s", body)
		}

		var forwarded struct {
			Context callcontext.Snapshot `json:"context"`
		}
		content := result.Content[0].(*mcpsdk.TextContent)
		if err := json.Unmarshal([]byte(content.Text), &forwarded); err != nil {
			t.Fatal(err)
		}
		if !forwarded.Context.Verified || forwarded.Context.SessionID != id || forwarded.Context.PrincipalID != "session:"+id {
			t.Fatalf("wrong forwarded session: %+v", forwarded.Context)
		}
		var telemetry events.ToolCallEvent
		for telemetry.ToolName == "" {
			select {
			case event := <-stream:
				if event.Kind == events.EventTypeToolCallEnd {
					if err := json.Unmarshal([]byte(event.PayloadJSON), &telemetry); err != nil {
						t.Fatal(err)
					}
				}
			case <-ctx.Done():
				t.Fatal("daemon telemetry lost")
			}
		}
		rows, err := f.db.QueryProxyEvents(store.ProxyEventFilter{SessionID: id, ToolName: "app_echo"})
		if err != nil || len(rows) != 1 {
			t.Fatalf("durable attribution lost/duplicated: %+v %v", rows, err)
		}
		if rows[0].Attribution != forwarded.Context || telemetry.Attribution != forwarded.Context {
			t.Fatalf("forwarded/persisted/telemetry attribution diverged: %+v %+v %+v", forwarded.Context, rows[0].Attribution, telemetry.Attribution)
		}
		_ = session.Close()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("relay leaked")
		}
	}
	raw, err := os.ReadFile(f.childStarts)
	if err != nil || string(raw) != "started\n" {
		t.Fatalf("thin proxies spawned multiple upstreams: %q %v", raw, err)
	}
}
