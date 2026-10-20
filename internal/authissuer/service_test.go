package authissuer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

var operatorP = identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}

func TestIssueServiceClaimsAndVerification(t *testing.T) {
	is, _ := newIssuer(t)
	tok, rec, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events", "tether.read"}, Label: "agent-os cerberus"})
	if err != nil {
		t.Fatal(err)
	}
	c := claimsOf(t, tok.Token)
	if c.Kind != "service" || c.Sub != "msg://service/cerberus/dev" || c.Aud != "tether" || c.Par != identity.OperatorID || c.JTI == "" {
		t.Fatalf("claims %+v", c)
	}
	if c.Scope != "tether.events tether.read" {
		t.Fatalf("scope %q", c.Scope)
	}
	// "No expiry" is a very distant exp, far past the 60 minute cap and not below 9 years.
	if got := time.Unix(c.Exp, 0).Sub(time.Unix(c.Iat, 0)); got < 9*365*24*time.Hour {
		t.Fatalf("exp only %s after iat", got)
	}
	if rec.JTI != c.JTI || rec.Sub != c.Sub || rec.IssuedBy != identity.OperatorID {
		t.Fatalf("record %+v", rec)
	}
	// It verifies through the same composite verifier the daemon uses, as a service principal.
	v, err := NewVerifier(fakeStored{map[string]identity.Principal{}}, is)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), tok.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "service" || p.ID != "msg://service/cerberus/dev" || p.TokenID != c.JTI || !identity.ScopeGranted(p.Scopes, "events") {
		t.Fatalf("principal %+v", p)
	}
	// The cap bypass is for service tokens only: Exchange still refuses a long ttl,
	// and an issued service token cannot mint anything.
	if _, err := is.Exchange(operatorP, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}, TTL: 2 * time.Hour}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("exchange cap changed: %v", err)
	}
	if _, err := is.Exchange(p, Request{Audience: "tether", Scopes: []string{"read"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("issued service token minted: %v", err)
	}
}

func TestIssueServiceRules(t *testing.T) {
	is, _ := newIssuer(t)
	device := identity.Principal{Verified: true, ID: "msg://device/d1", Kind: "device", Scopes: []string{"read", "operate", "admin"}}
	service := identity.Principal{Verified: true, ID: "msg://service/x/dev", Kind: "service", Scopes: []string{"*"}}
	agent := identity.Principal{Verified: true, ID: "a", Kind: "agent", Scopes: []string{"*"}}
	issued := operatorP
	issued.TokenID = "j1"
	unverified := operatorP
	unverified.Verified = false
	ok := ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}}
	for _, c := range []struct {
		name   string
		parent identity.Principal
		req    ServiceRequest
		want   error
	}{
		{"operator ok", operatorP, ok, nil},
		{"device refused", device, ok, ErrForbidden},
		{"service refused", service, ok, ErrForbidden},
		{"agent refused", agent, ok, ErrForbidden},
		{"issued operator token refused", issued, ok, ErrForbidden},
		{"unverified refused", unverified, ok, ErrForbidden},
		{"bad app", operatorP, ServiceRequest{App: "Bad App", Audience: "tether", Scopes: []string{"events"}}, ErrInvalid},
		{"empty app", operatorP, ServiceRequest{Audience: "tether", Scopes: []string{"events"}}, ErrInvalid},
		{"bad env", operatorP, ServiceRequest{App: "cerberus", Env: "Prod!", Audience: "tether", Scopes: []string{"events"}}, ErrInvalid},
		{"bad audience", operatorP, ServiceRequest{App: "cerberus", Audience: "a b", Scopes: []string{"events"}}, ErrInvalid},
		{"no audience", operatorP, ServiceRequest{App: "cerberus", Scopes: []string{"events"}}, ErrInvalid},
		{"no scopes", operatorP, ServiceRequest{App: "cerberus", Audience: "tether"}, ErrInvalid},
		{"tether admin refused", operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"admin"}}, ErrInvalid},
		{"tether operate refused", operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"tether.operate"}}, ErrInvalid},
		{"tether sessions refused", operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"sessions"}}, ErrInvalid},
		{"tether unknown scope", operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"bogus"}}, ErrInvalid},
		{"tether wildcard refused", operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"tether.*"}, AllowWildcard: true}, ErrInvalid},
		{"other app explicit verb", operatorP, ServiceRequest{App: "tachyon", Audience: "tachyon", Scopes: []string{"tachyon.approvals_enqueue"}}, nil},
		{"other app wrong prefix", operatorP, ServiceRequest{App: "tachyon", Audience: "tachyon", Scopes: []string{"torque.read"}}, ErrInvalid},
		{"wildcard without flag", operatorP, ServiceRequest{App: "tachyon", Audience: "tachyon", Scopes: []string{"tachyon.*"}}, ErrInvalid},
		{"wildcard with flag (dev)", operatorP, ServiceRequest{App: "tachyon", Audience: "tachyon", Scopes: []string{"tachyon.*"}, AllowWildcard: true}, nil},
		{"wildcard of another app", operatorP, ServiceRequest{App: "tachyon", Audience: "tachyon", Scopes: []string{"torque.*"}, AllowWildcard: true}, ErrInvalid},
		{"reserved audience", operatorP, ServiceRequest{App: "cerberus", Audience: "tachyon-enrollment", Scopes: []string{"tachyon-enrollment.x"}}, ErrInvalid},
		{"bad label", operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}, Label: "a\nb"}, ErrInvalid},
	} {
		_, _, err := is.IssueService(c.parent, c.req)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", c.name, err, c.want)
		}
	}
}

func TestServiceRecordsAreMetadataOnlyAndSurviveRestart(t *testing.T) {
	is, dir := newIssuer(t)
	tok, _, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}, Label: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := is.IssueService(operatorP, ServiceRequest{App: "nanite", Env: "staging", Audience: "tether", Scopes: []string{"events"}}); err != nil {
		t.Fatal(err)
	}
	list := is.State.Services()
	if len(list) != 2 || list[0].App != "cerberus" || list[1].Sub != "msg://service/nanite/staging" {
		t.Fatalf("%+v", list)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	sig := tok.Token[strings.LastIndex(tok.Token, ".")+1:]
	if strings.Contains(string(raw), tok.Token) || strings.Contains(string(raw), sig) {
		t.Fatal("state file holds the token")
	}
	// A new State over the same file still lists them.
	st2, err := OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Services(); len(got) != 2 {
		t.Fatalf("after restart: %+v", got)
	}
	// Expired records are pruned.
	st2.now = func() time.Time { return time.Now().Add(ServiceTokenLifetime + time.Hour) }
	if got := st2.Services(); len(got) != 0 {
		t.Fatalf("expired kept: %+v", got)
	}
}

func TestNothingIsMintedWhenTheRecordCannotBeSaved(t *testing.T) {
	is, dir := newIssuer(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make the state directory read-only")
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.OpenFile(filepath.Join(dir, "probe"), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		t.Skip("running with privileges that ignore directory modes")
	}
	tok, _, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
	if err == nil || tok.Token != "" {
		t.Fatalf("minted without a record: err=%v token-empty=%v", err, tok.Token == "")
	}
}

func TestServiceTokenCountIsBounded(t *testing.T) {
	is, _ := newIssuer(t)
	for i := 0; i < maxServiceRecords; i++ {
		if _, _, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}}); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
	if _, _, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("limit not enforced: %v", err)
	}
}

func TestRevokeServiceDeniesImmediatelyAndPersistsToExp(t *testing.T) {
	is, dir := newIssuer(t)
	tok, rec, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := is.IssueService(operatorP, ServiceRequest{App: "nanite", Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(fakeStored{map[string]identity.Principal{}}, is)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), tok.Token); err != nil {
		t.Fatalf("before revoke: %v", err)
	}
	_, etagBefore := is.State.DenyDoc()
	got, err := is.RevokeService(rec.JTI)
	if err != nil || got.RevokedAt == nil || got.JTI != rec.JTI {
		t.Fatalf("revoke: %+v %v", got, err)
	}
	// Tether's own verifier shares the list and refuses at once; the other token is untouched.
	if _, err := v.Verify(context.Background(), tok.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("revoked token still verifies: %v", err)
	}
	if _, err := v.Verify(context.Background(), other.Token); err != nil {
		t.Fatalf("another app's token was affected: %v", err)
	}
	body, etagAfter := is.State.DenyDoc()
	if etagAfter == etagBefore || !strings.Contains(string(body), rec.JTI) {
		t.Fatalf("deny-list did not change: %s", body)
	}
	// The entry lasts until the token's own exp, not the 65 minute sid horizon.
	for _, e := range is.State.doc.Deny {
		if e.Kind == DenyJTI && e.Value == rec.JTI && !e.Until.Equal(rec.ExpiresAt) {
			t.Fatalf("until %v, want exp %v", e.Until, rec.ExpiresAt)
		}
	}
	is.State.now = func() time.Time { return time.Now().Add(DenyHorizon + 24*time.Hour) }
	if !is.State.Denied(DenyJTI, rec.JTI) {
		t.Fatal("service token jti pruned from the deny-list at the sid horizon")
	}
	// It survives a restart; the record keeps the revocation time and the list shows it.
	st2, err := OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !st2.Denied(DenyJTI, rec.JTI) {
		t.Fatal("revocation lost on restart")
	}
	var revoked int
	for _, r := range st2.Services() {
		if r.RevokedAt != nil {
			revoked++
		}
	}
	if revoked != 1 {
		t.Fatalf("revoked records after restart: %d", revoked)
	}
	// Revoking again, or something unknown, is a clear not-found, not a silent success.
	if _, err := is.RevokeService(rec.JTI); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := is.RevokeService("nope"); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("unknown jti: %v", err)
	}
	if _, err := is.RevokeService(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty jti: %v", err)
	}
}

func TestRevokeServicesForAPrincipalRevokesAllItsTokens(t *testing.T) {
	is, _ := newIssuer(t)
	var jtis []string
	for i := 0; i < 3; i++ {
		_, rec, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
		if err != nil {
			t.Fatal(err)
		}
		jtis = append(jtis, rec.JTI)
	}
	_, stag, _ := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Env: "staging", Audience: "tether", Scopes: []string{"events"}})
	got, err := is.RevokeServicesFor("cerberus", "")
	if err != nil || len(got) != 3 {
		t.Fatalf("%d %v", len(got), err)
	}
	for _, j := range jtis {
		if !is.State.Denied(DenyJTI, j) {
			t.Errorf("%s not denied", j)
		}
	}
	if is.State.Denied(DenyJTI, stag.JTI) {
		t.Error("another env's token was revoked")
	}
	if _, err := is.RevokeServicesFor("cerberus", ""); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("nothing live left: %v", err)
	}
	if _, err := is.RevokeServicesFor("Bad App", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad app: %v", err)
	}
}
