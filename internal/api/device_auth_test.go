package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/a2aadapter"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func deviceAuthFixture(t *testing.T) (*identity.Store, *store.Store, string, http.Handler) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.NewStore(db.DB())
	op, err := ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	router := http.NewServeMux()
	router.Handle("/auth/", NewDeviceAuthHandler(ids))
	router.HandleFunc("/sessions", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	return ids, db, op, router
}

func authCall(t *testing.T, client *http.Client, base, method, path, token string, body, out any) int {
	t.Helper()
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, base+path, input)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(environment.ProtocolHeader, "1")
	r.Header.Set("User-Agent", "synthetic-worker/1")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal("synthetic identity request failed", err)
	}
	defer func() { _ = res.Body.Close() }()
	if out != nil && res.StatusCode < 300 {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal("invalid synthetic response", err)
		}
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
	}
	if len(path) >= 6 && path[:6] == "/auth/" && res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("credential response cacheable")
	}
	return res.StatusCode
}

func TestDevicePairingHTTPUnixAndRemoteLifecycle(t *testing.T) {
	ids, _, operator, router := deviceAuthFixture(t)
	socketDir, err := os.MkdirTemp(os.TempDir(), "t70-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	listener, err := net.Listen("unix", filepath.Join(socketDir, "operator.sock"))
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewUnstartedServer(identity.Middleware(identity.Enforce, ids, nil, router))
	_ = local.Listener.Close()
	local.Listener = listener
	local.Config.ConnContext = identity.ConnectionContext
	local.Start()
	defer local.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", listener.Addr().String())
	}}}
	defer client.CloseIdleConnections()
	remote := httptest.NewServer(identity.RemoteMiddleware(ids, nil, environment.ProtocolGate(true, RemoteScopeMiddleware(router))))
	defer remote.Close()
	var grant identity.IssuedGrant
	if status := authCall(t, client, "http://unix", "POST", "/auth/pair", operator, PairRequest{Label: "synthetic worker", Scopes: []string{"read", "operate"}}, &grant); status != 201 {
		t.Fatal("local pairing denied", status)
	}
	if status := authCall(t, remote.Client(), remote.URL, "POST", "/auth/pair", operator, PairRequest{Scopes: []string{"read"}}, nil); status != 401 {
		t.Fatal("remote operator accepted", status)
	}
	if status := authCall(t, remote.Client(), remote.URL, "POST", identity.PairingExchangePath, "", PairExchangeRequest{Code: grant.Code, Scopes: []string{"admin"}}, nil); status != 401 {
		t.Fatal("grant widening accepted", status)
	}
	var device identity.DeviceExchange
	if status := authCall(t, remote.Client(), remote.URL, "POST", identity.PairingExchangePath, "", PairExchangeRequest{Code: grant.Code, Scopes: []string{"read"}}, &device); status != 200 {
		t.Fatal("remote exchange failed", status)
	}
	if status := authCall(t, remote.Client(), remote.URL, "POST", identity.PairingExchangePath, "", PairExchangeRequest{Code: grant.Code}, nil); status != 401 {
		t.Fatal("grant replay accepted", status)
	}
	if status := authCall(t, remote.Client(), remote.URL, "GET", "/sessions", device.Token, nil, nil); status != 204 {
		t.Fatal("paired device read denied", status)
	}
	if status := authCall(t, remote.Client(), remote.URL, "POST", "/sessions", device.Token, map[string]string{"launch": "demo"}, nil); status != 403 {
		t.Fatal("read device mutation accepted", status)
	}
	if status := authCall(t, remote.Client(), remote.URL, "POST", "/auth/renew", device.Token, DeviceRenewRequest{Scopes: []string{"read"}}, nil); status != 200 {
		t.Fatal("valid device renewal failed", status)
	}
	var listed struct {
		Devices []identity.Device `json:"devices"`
	}
	if status := authCall(t, client, "http://unix", "GET", "/auth/devices", operator, nil, &listed); status != 200 || len(listed.Devices) != 1 || listed.Devices[0].LastUsedAt == nil {
		t.Fatal("local device list unavailable", status)
	}
	if status := authCall(t, client, "http://unix", "POST", "/auth/revoke", operator, DeviceRevokeRequest{ID: device.Principal.ID}, nil); status != 200 {
		t.Fatal("local revocation failed", status)
	}
	if status := authCall(t, remote.Client(), remote.URL, "GET", "/sessions", device.Token, nil, nil); status != 401 {
		t.Fatal("revoked device used API", status)
	}
}

func TestDeviceAuthRequiresAcceptedUnixOperatorAndBoundsExchange(t *testing.T) {
	_, _, _, router := deviceAuthFixture(t)
	// Even a valid operator on an ordinary local TCP connection cannot mint.
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	r := httptest.NewRequest("POST", "/auth/pair", bytes.NewBufferString(`{"scopes":["read"]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("request-supplied local proof accepted")
	}
	remote := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(identity.WithRemoteContext(r.Context())))
	})
	for i := range 11 {
		r := httptest.NewRequest("POST", identity.PairingExchangePath, bytes.NewBufferString(`{"code":"invalid"}`))
		w := httptest.NewRecorder()
		remote.ServeHTTP(w, r)
		want := 401
		if i == 10 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("bounded exchange %d returned %d", i, w.Code)
		}
	}
}

// 0074 still owns reconciling the inner A2A binding credential with devices.
// Neither the legacy binding bearer nor the scoped device bypasses a gate.
func TestDeviceRemoteA2AInnerBindingAuthenticationRemainsUnsupported(t *testing.T) {
	ids, db, _, _ := deviceAuthFixture(t)
	expires := time.Now().Add(time.Hour)
	device, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://device/a2a-fixture", Kind: "device", Scopes: []string{"read", "operate"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := ids.Mint(context.Background(), identity.Principal{ID: "a2a-binding", Kind: "service", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := a2aadapter.NewAdapter(a2aadapter.Config{Bindings: []a2aadapter.AgentBinding{{ID: "fixture", TargetURN: "msg://agent/local/fixture", BaseURL: "http://fixture.invalid/a2a", BearerToken: legacy}}}, db.MessagingStore())
	if err != nil {
		t.Fatal(err)
	}
	h := identity.RemoteMiddleware(ids, nil, RemoteScopeMiddleware(http.StripPrefix("/a2a", adapter.Tether())))
	for _, token := range []string{device, legacy} {
		r := httptest.NewRequest("POST", "/a2a/agents/fixture/rpc", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"synthetic"}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if token == legacy {
			if w.Code != 401 {
				t.Fatal("legacy binding bearer bypassed remote admission")
			}
			continue
		}
		var response struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Error == nil || response.Error.Message != "missing or invalid bearer token" {
			t.Fatal("inner binding authentication bypassed or incompatibility hidden")
		}
	}
}

func TestDeviceSelfNarrowingReturnsResponseAndLegacySessionIsRefused(t *testing.T) {
	ids, db, _, router := deviceAuthFixture(t)
	expires := time.Now().Add(time.Hour)
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://device/self", Kind: "device", Scopes: []string{"read", "operate"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: "legacy", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	legacy, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://session/local/legacy", Kind: "session", SessionID: "legacy", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(identity.RemoteMiddleware(ids, nil, RemoteScopeMiddleware(router)))
	defer server.Close()
	if status := authCall(t, server.Client(), server.URL, "POST", "/auth/renew", token, DeviceRenewRequest{Scopes: []string{"read"}}, nil); status != 200 {
		t.Fatal("self narrowing dropped response", status)
	}
	if status := authCall(t, server.Client(), server.URL, "GET", "/sessions", legacy, nil, nil); status != 401 {
		t.Fatal("legacy session used remote API", status)
	}
}

func TestPairingLimiterConcurrent(t *testing.T) {
	limiter := &pairingLimiter{}
	now := time.Now()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if limiter.allow(now) {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 10 {
		t.Fatal("concurrent exchange burst exceeded limit")
	}
	if !limiter.allow(now.Add(6*time.Second)) || limiter.allow(now.Add(6*time.Second)) {
		t.Fatal("refill window incorrect")
	}
}
