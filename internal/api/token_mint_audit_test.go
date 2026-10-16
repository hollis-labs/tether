package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

type auditRow struct{ principal, session, authn, method, route, scope, event string }

func TestSessionBoundMintWritesAnAuditRowForDeviceAndService(t *testing.T) {
	ids, db, _, _ := deviceAuthFixture(t)
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
	stored := tokenFake{
		"tth_dev": {ID: "msg://device/outpost", Kind: "device", Scopes: []string{"worker"}},
		"tth_svc": {ID: "msg://service/launcher", Kind: "service", Scopes: []string{"*"}},
		"tth_no":  {ID: "msg://service/ungranted", Kind: "service", Scopes: []string{"*"}},
	}
	v, err := authissuer.NewVerifier(stored, is)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(TokenPath, NewTokenIssuerHandler(is, WithMintAudit(ids.RecordObservation)))
	h := identity.Middleware(identity.Enforce, v, nil, mux)

	grant := map[string][]string{"tesseract": {"tesseract.read"}}
	for sid, parent := range map[string]string{"s-dev": "msg://device/outpost", "s-svc": "msg://service/launcher"} {
		if err := is.RegisterSessionGrant(sid, parent, grant, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	rows := func() []auditRow {
		t.Helper()
		q, err := db.DB().Query(`SELECT principal_id, session_id, authentication, method, route, required_scope, event FROM identity_audit WHERE event LIKE 'token_mint%' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer q.Close()
		var out []auditRow
		for q.Next() {
			var r auditRow
			if err := q.Scan(&r.principal, &r.session, &r.authn, &r.method, &r.route, &r.scope, &r.event); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	mintSid := func(bearer, sid string) (int, authissuer.Token) {
		return mint(t, h, bearer, fmt.Sprintf(`{"audience":"tesseract","scopes":["tesseract.read"],"session_id":%q}`, sid))
	}

	devCode, devTok := mintSid("tth_dev", "s-dev")
	svcCode, svcTok := mintSid("tth_svc", "s-svc")
	if devCode != 200 || svcCode != 200 {
		t.Fatalf("mints: %d %d", devCode, svcCode)
	}
	got := rows()
	if len(got) != 2 {
		t.Fatalf("want 2 audit rows, got %d: %+v", len(got), got)
	}
	for i, want := range []struct {
		par, sid string
		tok      authissuer.Token
	}{{"msg://device/outpost", "s-dev", devTok}, {"msg://service/launcher", "s-svc", svcTok}} {
		r := got[i]
		if r.principal != want.par || r.session != want.sid || r.authn != "verified" || r.method != "POST" || r.route != "/auth/token" || r.scope != "tesseract.read" {
			t.Errorf("row %d = %+v", i, r)
		}
		wantEvent := fmt.Sprintf("token_mint aud=tesseract jti=%s exp=%d", want.tok.JTI, want.tok.ExpiresAt.Unix())
		if want.tok.JTI == "" || r.event != wantEvent {
			t.Errorf("row %d event %q, want %q", i, r.event, wantEvent)
		}
		for _, f := range []string{r.principal, r.session, r.authn, r.method, r.route, r.scope, r.event} {
			if strings.Contains(f, want.tok.Token) || strings.Contains(f, "tth_") {
				t.Errorf("row %d carries credential material", i)
			}
		}
	}
	// no row for a non-sid mint, a refused mint, or a mint with no grant
	if c, _ := mint(t, h, "tth_svc", `{"audience":"tesseract","scopes":["tesseract.read"]}`); c != 200 {
		t.Fatal("non-sid mint", c)
	}
	if c, _ := mintSid("tth_no", "s-svc"); c != 403 {
		t.Fatal("ungranted service mint was not refused", c)
	}
	if c, _ := mintSid("tth_dev", "s-svc"); c != 403 {
		t.Fatal("device used another launcher's grant", c)
	}
	if n := len(rows()); n != 2 {
		t.Fatalf("non-sid or refused mints wrote audit rows: %d", n)
	}
}
