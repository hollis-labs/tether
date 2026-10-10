package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

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
	read := mint(identity.Principal{ID: "remote-reader", Kind: "service", Scopes: []string{"read"}})
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
