package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/util/localdaemon"
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestServeCommandPIDRecognition(t *testing.T) {
	savedVerify := verifyCommand
	t.Cleanup(func() { verifyCommand = savedVerify })
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"tether serve", true},
		{"/usr/local/bin/tether serve --remote-listen tcp:127.0.0.1:7331", true},
		{"./tether-dev --catalog /state/catalog serve", true},
		{"tether --catalog=/state/catalog --token-file=/state/token serve", true},
		{"tether --token-file= serve", true},
		{"tether daemon run --catalog /state/catalog", true},
		{"/usr/local/bin/tether --catalog /state/catalog daemon run", true},
		{"tether daemon start", false},
		{"tether daemon status", false},
		{"tether daemon stop", false},
		{"tether mcp", false},
		{"tether sessions serve", false},
		{"tether served", false},
		{"tether serve-other", false},
		{"tether --catalog=serve sessions", false},
		{"codex exec /usr/local/bin/tether serve", false},
		{"sh -c /usr/local/bin/tether daemon run", false},
		{"python /usr/local/bin/tether serve", false},
		{"/usr/local/bin/other-tether serve", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			verifyCommand = func(_ context.Context, _ int, matcher localdaemon.Matcher) (bool, error) {
				return matcher.MatchString(tc.command), nil
			}
			// Stop uses IsDaemon; start/status and PID writes use IsDaemonAlive.
			matched, err := IsDaemon(context.Background(), os.Getpid())
			if err != nil || matched != tc.want {
				t.Fatal("stop identity", matched, err)
			}
			if alive := IsDaemonAlive(os.Getpid()); alive != tc.want {
				t.Fatal("start/status identity", alive)
			}
		})
	}
}

func remoteTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.NewStore(db.DB())
	mint := func(p identity.Principal) string {
		t.Helper()
		token, err := ids.Mint(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	expires := time.Now().Add(time.Hour)
	read := mint(identity.Principal{ID: "remote-reader", Kind: "device", Scopes: []string{"read"}, ExpiresAt: &expires})
	operator := mint(identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	desc, err := environment.NewDescriptor(environment.Descriptor{EnvironmentID: "test-environment", Label: "test"})
	if err != nil {
		t.Fatal(err)
	}
	accepted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	s := &Server{
		Config: Config{IdentityMode: identity.Observe, RemoteListener: RemoteListenerConfig{
			Enabled: true, ListenAddr: "tcp:127.0.0.1:7331", AllowedHosts: []string{"localhost:9000"},
		}}, Identity: ids, Environment: desc, EnvironmentReport: accepted, A2A: accepted, MCP: accepted,
	}
	t.Cleanup(s.CloseIdentityAudit)
	return s, read, operator
}

func TestRemoteListenerAuthenticationAndLocalPolicy(t *testing.T) {
	s, read, operator := remoteTestServer(t)
	for _, mode := range []identity.Mode{identity.Off, identity.Observe, identity.Enforce} {
		s.Config.IdentityMode = mode
		local, remote := s.Handler(), s.RemoteHandler("127.0.0.1:7331")
		for _, tc := range []struct {
			path, token   string
			local, remote int
		}{
			{"/health", "", 200, 200},
			{environment.DescriptorPath, "", 200, 200},
			{"/a2a/", "", 202, 401},
			{"/a2a/", "invalid", 202, 401},
			{"/a2a/", operator, 202, 401},
			{"/a2a/", read, 202, 202},
			{"/v1/environment/report", read, 202, 202},
			{"/mcp", read, 202, 404},
			{"/p/example", read, 202, 404},
			{"/v1/environment/report", "", 202, 401},
			{"/v1/environment/report", "invalid", 202, 401},
			{"/v1/environment/report", operator, 202, 401},
		} {
			wantLocal := tc.local
			if mode == identity.Enforce && tc.path == "/v1/environment/report" && (tc.token == "" || tc.token == "invalid") {
				wantLocal = 401
			}
			for _, target := range []struct {
				name    string
				handler http.Handler
				want    int
			}{{"local", local, wantLocal}, {"remote", remote, tc.remote}} {
				r := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+tc.path, nil)
				if target.name == "remote" {
					r.Header.Set(environment.ProtocolHeader, "1")
				}
				r.Header.Set("X-Forwarded-User-Id", identity.OperatorID)
				if tc.token != "" {
					r.Header.Set("Authorization", "Bearer "+tc.token)
				}
				w := httptest.NewRecorder()
				target.handler.ServeHTTP(w, r)
				if w.Code != target.want {
					t.Fatalf("%s mode=%s %s status=%d want=%d", target.name, mode, tc.path, w.Code, target.want)
				}
				if target.name == "remote" && tc.path == "/health" {
					var health Health
					if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil || health.Identity.Mode != identity.Enforce {
						t.Fatal("remote health misstates enforce policy", err)
					}
				}
			}
		}
	}
}

func TestRemoteListenerHostOriginPolicy(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	for _, tc := range []struct {
		host    string
		origins []string
		want    int
	}{
		{"localhost:9000", nil, 200},
		{"localhost:9000", []string{"http://localhost:9000"}, 200},
		{"localhost:9001", nil, 403},
		{"LOCALHOST:9000", nil, 403},
		{"attacker.invalid", nil, 403},
		{"localhost:9000", []string{"http://attacker.invalid"}, 403},
		{"localhost:9000", []string{"null"}, 403},
		{"localhost:9000", []string{"http://localhost:9000/"}, 403},
		{"localhost:9000", []string{"http://localhost:9000", "http://localhost:9000"}, 403},
	} {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.Host = tc.host
		r.Header.Set("X-Forwarded-Host", "localhost:9000")
		r.Header.Set("X-Forwarded-Proto", "https")
		for _, origin := range tc.origins {
			r.Header.Add("Origin", origin)
		}
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("host=%s origins=%v status=%d want=%d", tc.host, tc.origins, w.Code, tc.want)
		}
	}
	s.Config.RemoteListener.AllowedOrigins = []string{"https://worker.example"}
	for _, origin := range []string{"https://worker.example", "http://localhost:9000"} {
		r := httptest.NewRequest(http.MethodGet, "http://localhost:9000/health", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		want := 403
		if origin == "https://worker.example" {
			want = 200
		}
		if w.Code != want {
			t.Fatal("explicit Origin allowlist not exact", w.Code)
		}
	}
}

func TestRemoteListenerProtocolAgreement(t *testing.T) {
	s, read, _ := remoteTestServer(t)
	for _, tc := range []struct {
		path, version, token string
		want                 int
	}{
		{"/a2a/", "", "", 401},
		{"/a2a/", "", read, 409},
		{"/a2a/", "2", read, 409},
		{"/a2a/", "1", read, 202},
		{"/a2a/?protocol=1", "", read, 202},
		{"/a2a/?protocol=2", "1", read, 409},
		{"/health", "2", "", 200},
		{environment.DescriptorPath, "2", "", 200},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://localhost:9000"+tc.path, nil)
		if tc.version != "" {
			r.Header.Set(environment.ProtocolHeader, tc.version)
		}
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(tc.path, tc.version, w.Code, tc.want)
		}
	}
}

func TestRemoteListenerStartupFailureReleasesLocalSocket(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	dir := shortTempDir(t)
	s.Config.ListenAddr = "unix:" + filepath.Join(dir, "local.sock")
	s.Config.PIDFile = filepath.Join(dir, "daemon.pid")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	s.Config.RemoteListener.ListenAddr = "tcp:" + lis.Addr().String()
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("remote bind collision accepted")
	}
	if _, err := os.Stat(SocketPath(s.Config.ListenAddr)); !os.IsNotExist(err) {
		t.Fatal("owned local socket survived startup failure", err)
	}
	if _, err := os.Stat(s.Config.PIDFile); !os.IsNotExist(err) {
		t.Fatal("PID written before both listeners opened", err)
	}
	conn, err := net.DialTimeout("tcp", lis.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("startup failure disturbed another listener", err)
	}
	_ = conn.Close()
}

func TestRemoteListenerRefusesMissingIdentityOrDisabledModule(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	dir := shortTempDir(t)
	s.Config.ListenAddr = "unix:" + filepath.Join(dir, "local.sock")
	s.Config.PIDFile = filepath.Join(dir, "daemon.pid")
	ids := s.Identity
	s.Identity = nil
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("remote started without verifier")
	}
	s.Identity = ids
	profile, err := environment.ResolveProfile("worker", map[string]bool{environment.RemoteListener: false}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Config.Modules = profile
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("remote bypassed disabled module")
	}
	if _, err := os.Stat(SocketPath(s.Config.ListenAddr)); !os.IsNotExist(err) {
		t.Fatal("preflight opened local socket", err)
	}
}

func TestRemoteListenerRefusesUnsafeConfiguration(t *testing.T) {
	for _, addr := range []string{"tcp:0.0.0.0:7331", "tcp:[::]:7331", "tcp:192.168.1.2:7331", "tcp:localhost:7331", "tcp::7331", "unix:/socket", "tcp:127.0.0.1:http", "tcp:127.0.0.1:99999"} {
		if err := (RemoteListenerConfig{Enabled: true, ListenAddr: addr}).Validate(); err == nil {
			t.Fatal("unsafe bind accepted", addr)
		}
	}
	for _, addr := range []string{"tcp:127.0.0.1:7331", "tcp:[::1]:7331", "tcp:127.0.0.1:0"} {
		if err := (RemoteListenerConfig{Enabled: true, ListenAddr: addr}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"*", "*.example", "http://localhost", "localhost:bad", "localhost:9000/path", "user@localhost", "localhost:"} {
		if err := (RemoteListenerConfig{Enabled: true, ListenAddr: DefaultRemoteListenAddr, AllowedHosts: []string{host}}).Validate(); err == nil {
			t.Fatal("unsafe Host config accepted", host)
		}
	}
}

func TestRemoteListenerBothTransportsAndJoinedShutdown(t *testing.T) {
	s, read, operator := remoteTestServer(t)
	dir := shortTempDir(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	s.Config.ListenAddr = "unix:" + filepath.Join(dir, "local.sock")
	s.Config.PIDFile = filepath.Join(dir, "daemon.pid")
	s.Config.ShutdownTimeout = time.Second
	s.Config.RemoteListener.ListenAddr = "tcp:" + addr
	s.Config.RemoteListener.AllowedHosts = nil
	started := make(chan struct{})
	s.Startup = func(ctx context.Context) { close(started); <-ctx.Done() }
	closed := false
	s.Close = func() error { closed = true; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("listeners did not start")
	}
	local := DialHTTPClient(s.Config.ListenAddr)
	local.Timeout = time.Second
	remote := &http.Client{Timeout: time.Second}
	t.Cleanup(local.CloseIdleConnections)
	t.Cleanup(remote.CloseIdleConnections)
	for _, tc := range []struct {
		client      *http.Client
		base, token string
		want        int
	}{
		{local, BaseURL(s.Config.ListenAddr), "", 202},
		{local, BaseURL(s.Config.ListenAddr), operator, 202},
		{remote, "http://" + addr, "", 401},
		{remote, "http://" + addr, "invalid", 401},
		{remote, "http://" + addr, operator, 401},
		{remote, "http://" + addr, read, 202},
	} {
		r, err := http.NewRequest(http.MethodGet, tc.base+"/a2a/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		r.Header.Set(environment.ProtocolHeader, "1")
		resp, err := tc.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatal("transport auth", resp.StatusCode, tc.want)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
	// Put the consumed result back for the cleanup's join.
	done <- nil
	if !closed {
		t.Fatal("service close was skipped")
	}
	if _, err := os.Stat(s.Config.PIDFile); !os.IsNotExist(err) {
		t.Fatal("PID file survived", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("remote listener survived shutdown")
	}
	if conn, err := net.DialTimeout("unix", SocketPath(s.Config.ListenAddr), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("local listener survived shutdown")
	}
}

func TestRemoteListenerRefusesLegacyKinds(t *testing.T) {
	s, _, operator := remoteTestServer(t)
	for _, kind := range []string{"service", "interactive", "operator"} {
		token := operator
		if kind != "operator" {
			var err error
			token, err = s.Identity.Mint(context.Background(), identity.Principal{ID: "legacy-" + kind, Kind: kind, Scopes: []string{"*", "read"}})
			if err != nil {
				t.Fatal(err)
			}
		}
		r := httptest.NewRequest("GET", "http://localhost:9000/v1/environment/report", nil)
		r.Header.Set(environment.ProtocolHeader, "1")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("remote legacy kind accepted", kind, w.Code)
		}
	}
}

// Non-Tether verifiers fetch the issuer's JWKS and poll its deny-list with a
// plain HTTP client, so the remote listener must not demand the Tether-Protocol
// header for those reads (but still for everything else, including minting).
func TestRemoteListenerIssuerReadsNeedNoProtocolHeader(t *testing.T) {
	s, read, _ := remoteTestServer(t)
	dir := filepath.Join(t.TempDir(), "auth")
	keys, err := authissuer.OpenKeyStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Issuer = authissuer.New("tether", keys, state)
	do := func(method, path, version, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost:9000"+path, strings.NewReader(body))
		if version != "" {
			r.Header.Set(environment.ProtocolHeader, version)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/.well-known/jwks.json", "", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"OKP"`) || strings.Contains(w.Body.String(), `"d"`) {
		t.Fatalf("public JWKS without header/bearer: %d %s", w.Code, w.Body.String())
	}
	// The minimal deny-list read is public: no credential, no protocol header,
	// ids only, so any verifier can honor a revocation. Writing it is not.
	if w := do("GET", "/auth/denylist", "", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"jti"`) {
		t.Fatalf("public deny-list read returned %d: %s", w.Code, w.Body.String())
	}
	if w := do("GET", "/auth/denylist", "", read, ""); w.Code != 200 {
		t.Fatalf("deny-list read with a read-only device returned %d, want 200 (public)", w.Code)
	}
	if w := do("POST", "/auth/denylist", "1", read, `{"kind":"jti","value":"x"}`); w.Code != 403 && w.Code != 401 {
		t.Fatalf("deny-list WRITE by a read-only device returned %d", w.Code)
	}
	// Minting is Tether-client traffic and still needs the header.
	body := `{"audience":"tether","scopes":["tether.read"]}`
	if w := do("POST", "/auth/token", "", read, body); w.Code != 409 {
		t.Fatalf("token mint without header returned %d, want 409", w.Code)
	}
	if w := do("POST", "/auth/token", "1", read, body); w.Code != 200 {
		t.Fatalf("token mint with header returned %d: %s", w.Code, w.Body.String())
	}
	// The production shape: the public Host the hub allows, no Tether-Protocol
	// header (a go-auth verifier), the deny-list read with a maintain device.
	s.Config.RemoteListener.AllowedHosts = []string{"localhost:9000", "tether.example"}
	expires := time.Now().Add(time.Hour)
	maintain, err := s.Identity.Mint(context.Background(), identity.Principal{ID: "remote-denylist-poller", Kind: "device", Scopes: []string{"maintain"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/.well-known/jwks.json", "/auth/denylist"} {
		r := httptest.NewRequest("GET", "https://tether.example"+path, nil)
		r.Header.Set("Authorization", "Bearer "+maintain)
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:8948").ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("GET %s with Host tether.example and no protocol header returned %d: %s", path, w.Code, w.Body.String())
		}
	}
	// A different Host is still refused, JWKS included.
	r := httptest.NewRequest("GET", "http://attacker.invalid/.well-known/jwks.json", nil)
	w := httptest.NewRecorder()
	s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("unlisted Host reached the JWKS: %d", w.Code)
	}
}

type fakeRemoteIngest struct {
	calls  int
	caller eventingest.Caller
}

func (f *fakeRemoteIngest) Ingest(_ context.Context, c eventingest.Caller, items []json.RawMessage) ([]ingest.Result, error) {
	f.calls++
	f.caller = c
	out := make([]ingest.Result, len(items))
	for i := range items {
		out[i] = ingest.Result{ID: "evt_x", Status: ingest.StatusAccepted, Cursor: "1"}
	}
	return out, nil
}

// An app publishes events with a plain HTTP client that cannot send the
// Tether-Protocol header (libs mesh/eventbus). POST /events is on the closed
// exemption list, behind authentication; nothing else a device can call is.
func TestRemoteListenerEventsPostNeedsNoProtocolHeaderButStillNeedsAuth(t *testing.T) {
	s, read, _ := remoteTestServer(t)
	ing := &fakeRemoteIngest{}
	s.EventIngest = ing
	expires := time.Now().Add(time.Hour)
	worker, err := s.Identity.Mint(context.Background(), identity.Principal{ID: "remote-worker", Kind: "device", Scopes: []string{"worker"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, version, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost:9000"+path, strings.NewReader(body))
		if version != "" {
			r.Header.Set(environment.ProtocolHeader, version)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		return w
	}
	const event = `{"schema_version":"1","id":"evt_00000000000000000000000000000001"}`
	if w := do("POST", "/events", "", worker, event); w.Code != 200 || ing.calls != 1 {
		t.Fatalf("worker device, no protocol header: %d %s (ingest calls %d)", w.Code, w.Body.String(), ing.calls)
	}
	if w := do("POST", "/events", "1", worker, event); w.Code != 200 {
		t.Fatalf("worker device with the header: %d", w.Code)
	}
	// Still authenticated and scoped: no credential, or a read-only device, never reaches the ingest.
	before := ing.calls
	if w := do("POST", "/events", "", "", event); w.Code != 401 {
		t.Fatalf("anonymous POST /events: %d", w.Code)
	}
	if w := do("POST", "/events", "", read, event); w.Code != 403 {
		t.Fatalf("read-only device POST /events: %d", w.Code)
	}
	if ing.calls != before {
		t.Fatal("an unauthenticated or unscoped request reached the ingest")
	}
	// The exemption is for this one route: a device still needs the header elsewhere.
	if w := do("POST", "/messages", "", worker, "{}"); w.Code != 409 {
		t.Fatalf("POST /messages without the header: %d, want 409", w.Code)
	}
	if w := do("GET", "/events", "", worker, ""); w.Code != 409 {
		t.Fatalf("GET /events without the header: %d, want 409", w.Code)
	}
}

// A service JWT issued by this daemon's issuer may POST /events on the remote
// listener and nowhere else; a device credential keeps working on the same route.
func TestRemoteListenerAcceptsAServiceJWTOnlyOnPostEvents(t *testing.T) {
	s, read, _ := remoteTestServer(t)
	dir := filepath.Join(t.TempDir(), "auth")
	keys, err := authissuer.OpenKeyStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.Issuer = authissuer.New("tether", keys, state)
	ing := &fakeRemoteIngest{}
	s.EventIngest = ing
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	issue := func(app string, scopes ...string) (string, authissuer.ServiceRecord) {
		t.Helper()
		tok, rec, err := s.Issuer.IssueService(op, authissuer.ServiceRequest{App: app, Audience: "tether", Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token, rec
	}
	svc, rec := issue("cerberus", "events")
	readOnly, _ := issue("nanite", "tether.read")
	exp := time.Now().Add(time.Hour)
	worker, err := s.Identity.Mint(context.Background(), identity.Principal{ID: "remote-worker", Kind: "device", Scopes: []string{"worker"}, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, target, version, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost:9000"+target, strings.NewReader(`{"schema_version":"1"}`))
		if version != "" {
			r.Header.Set(environment.ProtocolHeader, version)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		return w
	}

	// Accepted: no protocol header needed, bound to its app, actor is the service.
	if w := do("POST", "/events", "", svc); w.Code != 200 || ing.calls != 1 {
		t.Fatalf("service JWT POST /events: %d %s (calls %d)", w.Code, w.Body.String(), ing.calls)
	}
	if ing.caller.App != "cerberus" {
		t.Fatalf("caller app %q", ing.caller.App)
	}
	if ing.caller.Actor.URN != "msg://service/cerberus/dev" || ing.caller.Actor.Verified != "token" || ing.caller.Actor.Kind != "service" {
		t.Fatalf("actor %+v", ing.caller.Actor)
	}
	// And the device credential still works on the same route.
	if w := do("POST", "/events", "", worker); w.Code != 200 || ing.calls != 2 {
		t.Fatalf("device POST /events: %d (calls %d)", w.Code, ing.calls)
	}
	before := ing.calls
	// Wrong scope: 403. Revoked: 401 at once.
	if w := do("POST", "/events", "", readOnly); w.Code != 403 {
		t.Fatalf("service JWT without tether.events: %d", w.Code)
	}
	if _, err := s.Issuer.RevokeService(rec.JTI); err != nil {
		t.Fatal(err)
	}
	if w := do("POST", "/events", "", svc); w.Code != 401 {
		t.Fatalf("revoked service JWT: %d", w.Code)
	}
	// Everywhere else the (other, live) service JWT is refused, with and without the header.
	live, _ := issue("cerberus", "events")
	for _, c := range []struct{ method, target string }{
		{"GET", "/events"}, {"POST", "/events/"}, {"POST", "/events?x=1"}, {"POST", "/messages/inbox"}, {"GET", "/messages"}, {"GET", "/sessions"},
		{"GET", "/environment/events"}, {"POST", "/auth/token"}, {"POST", "/auth/pair"}, {"GET", "/auth/denylist/x"}, {"POST", "/mcp"},
	} {
		for _, v := range []string{"", "1"} {
			if w := do(c.method, c.target, v, live); w.Code != 401 && w.Code != 404 && w.Code != 409 {
				t.Errorf("%s %s (header %q) with a service JWT returned %d", c.method, c.target, v, w.Code)
			} else if w.Code == 200 || w.Code == 204 {
				t.Errorf("%s %s reached a handler", c.method, c.target)
			}
		}
	}
	if ing.calls != before {
		t.Fatal("a refused service JWT reached the ingest")
	}
	_ = read

	// A tether.messages service JWT is admitted on the two mail routes (and needs the
	// protocol header there, unlike /events); an events-only one is 403, and the
	// messages one is refused on /events.
	mail, _ := issue("tangent", "tether.messages")
	for _, target := range []string{"/messages", "/messages/notify"} {
		if w := do("POST", target, "", mail); w.Code != 409 {
			t.Errorf("messages JWT POST %s without the protocol header: %d, want 409", target, w.Code)
		}
		if w := do("POST", target, "1", mail); w.Code == 401 || w.Code == 403 {
			t.Errorf("messages JWT POST %s: %d, want it admitted", target, w.Code)
		}
		if w := do("POST", target, "1", live); w.Code != 403 {
			t.Errorf("events-only JWT POST %s: %d, want 403", target, w.Code)
		}
	}
	if w := do("POST", "/events", "", mail); w.Code != 403 {
		t.Errorf("messages-only JWT POST /events: %d, want 403", w.Code)
	}
	for _, c := range []struct{ method, target string }{{"GET", "/messages/inbox"}, {"GET", "/messages"}, {"POST", "/messages/x/consume"}, {"POST", "/sessions"}} {
		if w := do(c.method, c.target, "1", mail); w.Code != 401 && w.Code != 404 {
			t.Errorf("messages JWT %s %s: %d, want refused", c.method, c.target, w.Code)
		}
	}

	// Without the ingest on, a service JWT is just another refused bearer.
	s.EventIngest = nil
	if w := do("POST", "/events", "", live); w.Code != 401 {
		t.Fatalf("service JWT with the ingest off: %d", w.Code)
	}
}
