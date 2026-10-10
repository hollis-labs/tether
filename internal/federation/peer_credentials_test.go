package federation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
)

type credentialTestTransport func(*http.Request) (*http.Response, error)

func (f credentialTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func peerTokenFile(t *testing.T) (string, string) {
	t.Helper()
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "peer.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path, token
}

func TestPeerCredentialReferenceValidation(t *testing.T) {
	for _, ref := range []string{"literal-secret", "file://host/token", "file://relative", "file:///", "file:///token?x=y", "file:///token#fragment", "helper://issuer/token", "${TOKEN}"} {
		if _, err := credentialPath(ref); !errors.Is(err, ErrPeerCredential) {
			t.Fatalf("reference %q accepted: %v", ref, err)
		}
	}
	path, err := credentialPath("file:///private/peer%20token")
	if err != nil || path != "/private/peer token" {
		t.Fatal(path, err)
	}
	for _, endpoint := range []string{"http://192.0.2.1:80", "https://127.0.0.1:80", "http://user:secret@127.0.0.1:80", "http://127.0.0.1:80?token=x", "http://127.0.0.1:80#x"} {
		base, _ := url.Parse(endpoint)
		if err := validatePeerCredential(base, "file:///private/token"); err == nil {
			t.Fatalf("endpoint accepted: %s", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:7331", "http://[::1]:7331", "http://localhost:7331"} {
		base, _ := url.Parse(endpoint)
		if err := validatePeerCredential(base, "file:///private/token"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPeerCredentialClientResolvesAtUseAndDoesNotMutateCaller(t *testing.T) {
	path, token := peerTokenFile(t)
	got := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		if r.Header.Get(environment.ProtocolHeader) != "1" {
			t.Error("protocol header missing")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	caller := &http.Client{Transport: transport, Timeout: 7 * time.Second}
	base, _ := url.Parse(server.URL)
	client, err := peerCredentialClient(caller, "worker", base, "file://"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if client == caller || caller.Timeout != client.Timeout || caller.Transport != transport || transport.DialContext != nil {
		t.Fatal("client/shared transport mutated")
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/messages/inbox", nil)
	request := func() {
		t.Helper()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	request()
	if <-got != "Bearer "+token {
		t.Fatal("initial credential missing")
	}
	newToken, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(newToken), 0600); err != nil {
		t.Fatal(err)
	}
	request()
	if <-got != "Bearer "+newToken || req.Header.Get("Authorization") != "" {
		t.Fatal("credential not refreshed or request mutated")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); !errors.Is(err, ErrPeerCredential) {
		t.Fatal(err)
	}
	select {
	case <-got:
		t.Fatal("insecure file reached endpoint")
	default:
	}
}

func TestPeerCredentialFailuresNeverSendOrExposeSecrets(t *testing.T) {
	path, token := peerTokenFile(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path + "-missing", link} {
		base, _ := url.Parse("http://127.0.0.1:7331")
		client, err := peerCredentialClient(&http.Client{}, "worker", base, "file://"+file)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Get(base.String())
		var auth *PeerAuthError
		if !errors.Is(err, ErrPeerCredential) || !errors.As(err, &auth) || strings.Contains(err.Error(), file) || strings.Contains(err.Error(), token) {
			t.Fatal("unsafe or untyped credential error", err)
		}
	}
	transport := &peerCredentialTransport{base: credentialTestTransport(func(r *http.Request) (*http.Response, error) { return nil, errors.New(r.Header.Get("Authorization")) }), authority: "worker", scheme: "http", host: "127.0.0.1:7331", path: path}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:7331", nil)
	if _, err := transport.RoundTrip(req); err == nil || strings.Contains(err.Error(), token) {
		t.Fatal("transport exposed secret", err)
	}
}

func TestPeerCredentialRedirectCannotSendToAnotherOrigin(t *testing.T) {
	path, _ := peerTokenFile(t)
	foreignCalls := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls++; w.WriteHeader(204) }))
	defer foreign.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/messages", http.StatusTemporaryRedirect)
	}))
	defer peer.Close()
	base, _ := url.Parse(peer.URL)
	caller := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	client, err := peerCredentialClient(caller, "worker", base, "file://"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Get(peer.URL + "/messages")
	if !errors.Is(err, ErrPeerOrigin) || foreignCalls != 0 {
		t.Fatalf("redirect = %v, foreign calls = %d", err, foreignCalls)
	}
}

func TestPeerCredentialRejectsUnverifiableTransportAndWrongDialEndpoint(t *testing.T) {
	path, _ := peerTokenFile(t)
	base, _ := url.Parse("http://127.0.0.1:7331")
	if _, err := peerCredentialClient(&http.Client{Transport: credentialTestTransport(func(*http.Request) (*http.Response, error) { t.Fatal("unsupported transport reached"); return nil, nil })}, "worker", base, "file://"+path); err == nil {
		t.Fatal("unverifiable transport accepted")
	}
	foreignCalls := 0
	foreign := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls++; w.WriteHeader(204) }))
	_ = foreign.Listener.Close()
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	foreign.Listener = listener
	foreign.Start()
	defer foreign.Close()
	wrong, _ := url.Parse(foreign.URL)
	supplied := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, wrong.Host)
	}}
	defer supplied.CloseIdleConnections()
	for _, endpoint := range []string{base.String(), "http://127.0.0.1:" + wrong.Port()} {
		base, _ := url.Parse(endpoint)
		client, err := peerCredentialClient(&http.Client{Transport: supplied}, "worker", base, "file://"+path)
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		if _, err := client.Get(base.String()); !errors.Is(err, ErrPeerOrigin) || foreignCalls != 0 {
			t.Fatal("custom dial escaped endpoint", err, foreignCalls)
		}
	}
}

func TestAuthenticatedPeerErrorsDoNotReflectCredentialMaterial(t *testing.T) {
	path, token := peerTokenFile(t)
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, r.Header.Get("Authorization"))
			}))
			defer peer.Close()
			client, err := HTTPDialer(nil)(Peer{Authority: "worker", BaseURL: peer.URL, CredentialRef: "file://" + path})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Inbox(context.Background(), addr("worker", "recipient"), messaging.Filter{})
			if err == nil || strings.Contains(err.Error(), token) {
				t.Fatal("response reflected credential")
			}
			if status == 401 && !errors.Is(err, ErrPeerAuthentication) {
				t.Fatal("untyped authentication error")
			}
			if status == 403 && !errors.Is(err, ErrPeerScope) {
				t.Fatal("untyped authorization error")
			}
		})
	}
}

func TestPeerCredentialLocalhostAndProxyGuard(t *testing.T) {
	path, _ := peerTokenFile(t)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer peer.Close()
	base, _ := url.Parse(peer.URL)
	base.Host = net.JoinHostPort("localhost", base.Port())
	proxyCalled := false
	transport := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { proxyCalled = true; return url.Parse("http://192.0.2.1:8888") }}
	defer transport.CloseIdleConnections()
	client, err := peerCredentialClient(&http.Client{Transport: transport}, "worker", base, "file://"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(base.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if proxyCalled || transport.Proxy == nil {
		t.Fatal("proxy used or supplied transport mutated")
	}
}
