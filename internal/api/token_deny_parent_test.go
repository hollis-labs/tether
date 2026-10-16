package api

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

func TestDenyKindParentDeniesIssuedTokensForTheLocalOperatorOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	ks, err := authissuer.OpenKeyStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	is := authissuer.New("tether", ks, st)
	h := NewTokenIssuerHandler(is)
	ctx := localCtx(t)
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	narrow := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"tether.read"}, TokenID: "jti-x"}
	for i := 0; i < 3; i++ {
		if _, err := is.Exchange(op, authissuer.Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(p identity.Principal, body string) *httptest.ResponseRecorder {
		r := reqAs(ctx, p)
		r.Method, r.URL.Path = "POST", "/auth/denylist"
		r.Body = httptest.NewRequest("POST", "/", strings.NewReader(body)).Body
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(narrow, `{"kind":"par","value":"`+identity.OperatorID+`"}`); w.Code != 403 {
		t.Fatalf("an operator-minted narrow token reached the deny-list write: %d", w.Code)
	}
	if w := post(op, `{"kind":"par","value":""}`); w.Code != 400 {
		t.Fatalf("empty parent returned %d", w.Code)
	}
	w := post(op, `{"kind":"par","value":"`+identity.OperatorID+`"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tokens":3`) {
		t.Fatalf("deny by parent: %d %s", w.Code, w.Body.String())
	}
	body, _ := st.DenyDoc()
	if strings.Count(string(body), `"jti":[`) != 1 || strings.Contains(string(body), `"jti":[]`) {
		t.Fatalf("deny-list document has no jtis: %s", body)
	}
}
