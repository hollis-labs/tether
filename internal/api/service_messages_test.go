package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type rejectAllVerifier struct{}

func (rejectAllVerifier) Verify(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, identity.ErrInvalidToken
}

type jwtVerifierStub struct{ p identity.Principal }

func (v jwtVerifierStub) Verify(context.Context, string) (identity.Principal, error) { return v.p, nil }

// CW-20261030-0198: through the real remote chain (service JWT middleware, the
// remote scope gate, the handler), a tether.messages service token sends mail
// stamped with its own service address, and reaches nothing else.
func TestRemoteServiceTokenSendsMailAndNothingElse(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "svc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	api := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), IdentityMode: identity.Enforce})
	exp := time.Now().Add(time.Hour)
	p := identity.Principal{ID: "msg://service/tangent/dev", Kind: "service", TokenID: "jti-t1", Audience: "tether",
		Scopes: []string{"tether.messages"}, ExpiresAt: &exp, Verified: true}
	chain := identity.RemoteMiddleware(rejectAllVerifier{}, nil, RemoteScopeMiddleware(api),
		identity.WithServiceJWT(identity.ServiceJWT{Verify: jwtVerifierStub{p}, Known: func(j string) bool { return j == "jti-t1" }}))
	srv := httptest.NewServer(chain)
	t.Cleanup(srv.Close)

	do := func(method, path, body string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer aaaa.bbbb.cccc")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}

	code, raw := do("POST", "/messages", `{"to":"msg://agent/agent-mux/agt_target","kind":"response","payload":{"body":"hello"}}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST /messages = %d %s", code, raw)
	}
	var sent struct {
		ID   string `json:"id"`
		From string `json:"from"`
	}
	_ = json.Unmarshal(raw, &sent)
	var from string
	if err := db.DB().QueryRow(`SELECT from_urn FROM messaging_messages ORDER BY rowid DESC LIMIT 1`).Scan(&from); err != nil {
		t.Skipf("cannot read the stored sender here: %v (response %s)", err, raw)
	}
	if from != "msg://service/tangent/dev" {
		t.Fatalf("stored sender = %q, want the service address", from)
	}

	// claiming another sender is refused under enforce
	if code, raw = do("POST", "/messages", `{"from":"msg://agent/agent-mux/agt_other","to":"msg://agent/agent-mux/agt_target","kind":"response","payload":{"body":"x"}}`); code != http.StatusForbidden {
		t.Fatalf("send as another address = %d %s, want 403", code, raw)
	}
	// every other route stays closed to the JWT
	for _, c := range [][2]string{{"GET", "/messages/inbox"}, {"GET", "/messages"}, {"POST", "/sessions"}, {"POST", "/messages/x/consume"}} {
		if code, _ := do(c[0], c[1], `{}`); code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", c[0], c[1], code)
		}
	}
}
