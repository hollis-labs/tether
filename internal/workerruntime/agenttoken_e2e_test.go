package workerruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/libs/security/secrets/tokenfile"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type deviceVerifier map[string]identity.Principal

func (d deviceVerifier) Verify(_ context.Context, t string) (identity.Principal, error) {
	if p, ok := d[t]; ok {
		return p, nil
	}
	return identity.Principal{}, identity.ErrInvalidToken
}

// End to end on temp dirs and ephemeral ports: a hub (issuer + deny-list over
// Tether's own bearer auth), an outpost executor keeping a session token file,
// and a separate "app" protected by go-auth that polls the hub's JWKS and
// deny-list.
func TestAgentTokenFileRefreshedCallsProtectedAppAndRevokes(t *testing.T) {
	root := t.TempDir()
	keys, err := authissuer.OpenKeyStore(filepath.Join(root, "hub", "auth"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(root, "hub", "auth", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	issuer := authissuer.New("tether-e2e", keys, state)

	outpostToken, _ := identity.NewToken()
	maintainToken, _ := identity.NewToken()
	stored := deviceVerifier{
		outpostToken:  {ID: "msg://device/outpost", Kind: "device", Scopes: []string{"worker"}},
		maintainToken: {ID: "msg://device/app-poller", Kind: "device", Scopes: []string{"maintain"}},
	}
	verifier, err := authissuer.NewVerifier(stored, issuer)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, p := range []string{api.TokenPath, api.DenyListPath, api.JWKSPath} {
		mux.Handle(p, api.NewTokenIssuerHandler(issuer))
	}
	// The hub's remote listener runs the protocol gate with require=true: the
	// outpost minter is a Tether client and must send the header, while the
	// go-auth verifier (JWKS fetch, deny-list poll) is a plain HTTP client and
	// must not need it.
	hub := httptest.NewServer(environment.ProtocolGate(true, identity.Middleware(identity.Enforce, verifier, nil, mux)))
	defer hub.Close()

	// The protected app: go-auth JWT verifier against the hub, deny-list poll at 100ms.
	deny := auth.NewDenyList()
	poller := &auth.DenyListPoller{URL: hub.URL + api.DenyListPath, List: deny, Interval: 100 * time.Millisecond,
		Header: http.Header{"Authorization": {"Bearer " + maintainToken}}}
	jv, err := auth.NewJWTVerifier(auth.JWTConfig{Issuer: "tether-e2e", Audience: "tesseract", JWKSURL: hub.URL + api.JWKSPath, DenyList: deny, MinRefresh: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)
	scopes := auth.NewScopeTable().Route("GET", "/notes", "tesseract.read").Route("POST", "/notes", "tesseract.write")
	guard, err := auth.New(auth.Config{Mode: auth.ModeEnforce, Verifier: jv, Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(guard.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		if p.SessionID != "sess-1" || p.Kind != "agent" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	defer app.Close()
	callApp := func(method, token string) int {
		req, _ := http.NewRequest(method, app.URL+"/notes", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// Hub registers the launch grant; the outpost keeps the token file.
	if err := issuer.RegisterSessionGrant("sess-1", "msg://device/outpost", map[string][]string{"tesseract": {"tesseract.read"}}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	deviceFile := filepath.Join(root, "outpost", "device.token")
	if err := os.MkdirAll(filepath.Dir(deviceFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := identity.WriteTokenFile(deviceFile, outpostToken); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{Tokens: &TokenMinter{Hub: hub.URL, DeviceTokenFile: deviceFile, TTL: 3 * time.Second}}
	authDir := filepath.Join(root, "outpost", "ws", "auth")
	env, stop, err := ex.startAgentTokens("sess-1", authDir, []workerwire.AgentTokenSpec{{Aud: "tesseract", Scopes: []string{"tesseract.read"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	path := TokenFilePath(authDir, "tesseract")
	if len(env) != 1 || env[0] != "HOLLIS_AUTH_TOKEN_FILE_TESSERACT="+path {
		t.Fatalf("env %v", env)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode().Perm())
	}
	first, err := tokenfile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := callApp("GET", first); got != http.StatusNoContent {
		t.Fatalf("protected endpoint with the injected token: %d", got)
	}
	if got := callApp("POST", first); got != http.StatusForbidden {
		t.Fatalf("write beyond the token's scope: %d", got)
	}

	// Refreshed in place (TTL 3s: rewritten between 4/9 and 2/3 of it).
	var second string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tok, err := tokenfile.Read(path); err == nil && tok != first {
			second = tok
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second == "" {
		t.Fatal("token file was not refreshed in place")
	}
	if got := callApp("GET", second); got != http.StatusNoContent {
		t.Fatalf("refreshed token rejected: %d", got)
	}

	// Revocation: the hub deny-lists the sid; the app refuses within its poll interval.
	if err := issuer.RevokeSession("sess-1"); err != nil {
		t.Fatal(err)
	}
	revoked := false
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if callApp("GET", second) == http.StatusUnauthorized {
			revoked = true
			break
		}
	}
	if !revoked {
		t.Fatal("revoked session token still accepted after the poll interval")
	}
	// and the outpost can no longer mint a replacement
	if _, err := ex.Tokens.Mint(context.Background(), "tesseract", []string{"tesseract.read"}, "sess-1"); err == nil {
		t.Fatal("hub minted for an ended session")
	}

	// Session end on the outpost removes the files.
	stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("token file survived session end")
	}
}

// JWKS rotation: the app picks up a token signed by a newly rotated key via the
// unknown-kid refetch and old tokens stay valid for the overlap window (key
// removal after the window is covered in authissuer).
func TestJWKSRotationAgainstGoAuthVerifier(t *testing.T) {
	keys, err := authissuer.OpenKeyStore(filepath.Join(t.TempDir(), "auth"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := authissuer.OpenState(filepath.Join(t.TempDir(), "state.json"))
	issuer := authissuer.New("tether-rot", keys, state)
	mux := http.NewServeMux()
	mux.Handle(api.JWKSPath, api.NewTokenIssuerHandler(issuer))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jv, err := auth.NewJWTVerifier(auth.JWTConfig{Issuer: "tether-rot", Audience: "tesseract", JWKSURL: srv.URL + api.JWKSPath, MinRefresh: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	mintTok := func() string {
		tok, err := issuer.Exchange(op, authissuer.Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}})
		if err != nil {
			t.Fatal(err)
		}
		return tok.Token
	}
	ctx := context.Background()
	oldTok := mintTok()
	if _, err := jv.Verify(ctx, oldTok); err != nil {
		t.Fatalf("pre-rotation: %v", err)
	}
	if err := issuer.Rotate(); err != nil {
		t.Fatal(err)
	}
	newTok := mintTok()
	if _, err := jv.Verify(ctx, newTok); err != nil {
		t.Fatalf("token from rotated key (unknown-kid refetch): %v", err)
	}
	if _, err := jv.Verify(ctx, oldTok); err != nil {
		t.Fatalf("old token during overlap: %v", err)
	}
}
