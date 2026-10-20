package authissuer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"

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

func verifierFor(t *testing.T, is *Issuer) *Verifier {
	t.Helper()
	v, err := NewVerifier(fakeStored{map[string]identity.Principal{}}, is)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestServiceTokensAreSignedByTheDedicatedKeyAndSurviveSessionKeyRotationAndPrune(t *testing.T) {
	is, _ := newIssuer(t)
	tok, rec, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.KID, ServiceKIDPrefix) || tok.KID != rec.KID {
		t.Fatalf("kid %q / %q", rec.KID, tok.KID)
	}
	if sk, _ := is.Keys.Active(); strings.HasPrefix(sk, ServiceKIDPrefix) {
		t.Fatalf("the session signing key is a service key: %s", sk)
	}
	// An ordinary token is never signed by the service key.
	ord, err := is.Exchange(operatorP, Request{Audience: "tether", Scopes: []string{"read"}})
	if err != nil || strings.HasPrefix(ord.KID, ServiceKIDPrefix) {
		t.Fatalf("session token kid %q err %v", ord.KID, err)
	}
	// Normal key rotation, then the clock past the overlap window and a prune
	// (what Maintain does hourly): the session keys go, the service token still verifies.
	if err := is.Rotate(); err != nil {
		t.Fatal(err)
	}
	is.Keys.now = func() time.Time { return time.Now().Add(DefaultKeyOverlap + time.Hour) }
	is.Now = is.Keys.now
	if _, err := is.Keys.PruneWith(is.ServiceKeyInUse); err != nil {
		t.Fatal(err)
	}
	if got := len(is.Keys.KIDs()); got != 2 { // the new session key + the service key
		t.Fatalf("keys after prune: %v", is.Keys.KIDs())
	}
	v := verifierFor(t, is)
	if _, err := v.Verify(context.Background(), tok.Token); err != nil {
		t.Fatalf("service token stopped verifying after a rotation and prune: %v", err)
	}
	if _, err := v.Verify(context.Background(), ord.Token); err == nil {
		t.Fatal("the pruned session key's token still verifies")
	}
}

func TestServiceKeyIsKeptWhileALiveRecordNamesItAndPrunableAfter(t *testing.T) {
	is, dir := newIssuer(t)
	_, rec, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	// Prune with no source of truth must keep every service key.
	if _, err := is.Keys.PruneWith(nil); err != nil {
		t.Fatal(err)
	}
	// Rotate the service key: the old one is retired but stays while the record is live.
	res, err := is.RotateServiceKey(operatorP, false)
	if err != nil || res.NewKID == rec.KID || len(res.Pending) != 1 || res.Pending[0].JTI != rec.JTI {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := is.Keys.PruneWith(is.ServiceKeyInUse); err != nil {
		t.Fatal(err)
	}
	if !containsKID(is.Keys.KIDs(), rec.KID) || !containsKID(is.Keys.KIDs(), res.NewKID) {
		t.Fatalf("old service key pruned while a live token needs it: %v", is.Keys.KIDs())
	}
	if _, err := os.Stat(filepath.Join(dir, rec.KID+".key")); err != nil {
		t.Fatal(err)
	}
	// After the last record naming it is revoked, the old key may go; the active service key never does.
	if _, err := is.RevokeService(rec.JTI); err != nil {
		t.Fatal(err)
	}
	// Revoking prunes at once: nothing is left for the hourly pass to do.
	if removed, err := is.Keys.PruneWith(is.ServiceKeyInUse); err != nil || removed {
		t.Fatalf("the revoke should already have dropped the key: removed=%v err=%v", removed, err)
	}
	if containsKID(is.Keys.KIDs(), rec.KID) {
		t.Fatal("old service key kept after its last record was revoked")
	}
	if _, err := os.Stat(filepath.Join(dir, rec.KID+".key")); !os.IsNotExist(err) {
		t.Fatalf("old key file still on disk: %v", err)
	}
	if !containsKID(is.Keys.KIDs(), res.NewKID) {
		t.Fatal("the active service key was pruned")
	}
	if removed, _ := is.Keys.PruneWith(is.ServiceKeyInUse); removed {
		t.Fatal("pruned the active service key with no records")
	}
	// A reopened store has the same keys.
	ks2, err := OpenKeyStore(dir, time.Hour)
	if err != nil || !containsKID(ks2.KIDs(), res.NewKID) {
		t.Fatalf("%v %v", ks2.KIDs(), err)
	}
}

func containsKID(kids []string, kid string) bool {
	for _, k := range kids {
		if k == kid {
			return true
		}
	}
	return false
}

func TestRotateServiceKeyKeepsOldTokensValidUntilRevokedAndReissuesOnRequest(t *testing.T) {
	is, _ := newIssuer(t)
	oldTok, oldRec, _ := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}, Label: "agent-os"})
	other, otherRec, _ := is.IssueService(operatorP, ServiceRequest{App: "nanite", Env: "staging", Audience: "tether", Scopes: []string{"tether.read", "events"}})
	revokedTok, revokedRec, _ := is.IssueService(operatorP, ServiceRequest{App: "hadron", Audience: "tether", Scopes: []string{"events"}})
	if _, err := is.RevokeService(revokedRec.JTI); err != nil {
		t.Fatal(err)
	}
	if _, err := is.RotateServiceKey(identity.Principal{Verified: true, ID: "d", Kind: "device", Scopes: []string{"admin"}}, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a device rotated the service key: %v", err)
	}
	res, err := is.RotateServiceKey(operatorP, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 2 || len(res.Reissued) != 2 {
		t.Fatalf("pending %d reissued %d (revoked record must not be re-issued)", len(res.Pending), len(res.Reissued))
	}
	v := verifierFor(t, is)
	ctx := context.Background()
	for name, tk := range map[string]string{"old cerberus": oldTok.Token, "old nanite": other.Token} {
		if _, err := v.Verify(ctx, tk); err != nil {
			t.Errorf("%s stopped verifying after the rotation: %v", name, err)
		}
	}
	if _, err := v.Verify(ctx, revokedTok.Token); err == nil {
		t.Error("a revoked token verifies after the rotation")
	}
	byReplaces := map[string]ReissuedToken{}
	for _, r := range res.Reissued {
		byReplaces[r.Record.Replaces] = r
		if r.Record.KID != res.NewKID || r.Record.JTI == r.Record.Replaces {
			t.Errorf("replacement %+v", r.Record)
		}
		p, err := v.Verify(ctx, r.Token.Token)
		if err != nil || p.ID != r.Record.Sub {
			t.Errorf("replacement does not verify: %v", err)
		}
	}
	if byReplaces[oldRec.JTI].Record.Label != "agent-os" || byReplaces[otherRec.JTI].Record.Sub != "msg://service/nanite/staging" ||
		strings.Join(byReplaces[otherRec.JTI].Record.Scopes, " ") != "tether.events tether.read" {
		t.Fatalf("replacements lost their identity: %+v", byReplaces)
	}
	// The operator delivers the new tokens, then revokes the old ones: old stop, new keep working.
	for _, j := range []string{oldRec.JTI, otherRec.JTI} {
		if _, err := is.RevokeService(j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.Verify(ctx, oldTok.Token); err == nil {
		t.Error("old token verifies after being revoked")
	}
	for _, r := range res.Reissued {
		if _, err := v.Verify(ctx, r.Token.Token); err != nil {
			t.Errorf("replacement stopped working: %v", err)
		}
	}
	// Rotate without re-issue only reports what needs re-delivery and mints nothing.
	again, err := is.RotateServiceKey(operatorP, false)
	if err != nil || len(again.Reissued) != 0 || len(again.Pending) != 2 {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestRevokeAllIsTheEmergencyStop(t *testing.T) {
	is, _ := newIssuer(t)
	var toks []string
	for _, a := range []string{"cerberus", "cerberus", "nanite", "hadron"} {
		tk, _, err := is.IssueService(operatorP, ServiceRequest{App: a, Audience: "tether", Scopes: []string{"events"}})
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, tk.Token)
	}
	got, err := is.RevokeAllServices("cerberus")
	if err != nil || len(got) != 2 {
		t.Fatalf("%d %v", len(got), err)
	}
	v := verifierFor(t, is)
	for i, tk := range toks {
		_, err := v.Verify(context.Background(), tk)
		if (i < 2) == (err == nil) {
			t.Errorf("token %d verifies=%v", i, err == nil)
		}
	}
	got, err = is.RevokeAllServices("")
	if err != nil || len(got) != 2 {
		t.Fatalf("revoke all: %d %v", len(got), err)
	}
	for i, tk := range toks {
		if _, err := v.Verify(context.Background(), tk); err == nil {
			t.Errorf("token %d still verifies after revoke --all", i)
		}
	}
	if _, err := is.RevokeAllServices(""); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("nothing left: %v", err)
	}
	if _, err := is.RevokeAllServices("Bad App"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad app: %v", err)
	}
}

func TestNoKeyMaterialInStateDumpsOrLogs(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	is, dir := newIssuer(t)
	if _, _, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := is.RotateServiceKey(operatorP, true); err != nil {
		t.Fatal(err)
	}
	var seeds []string
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".key") {
			b, _ := os.ReadFile(filepath.Join(dir, f.Name()))
			seeds = append(seeds, strings.TrimSpace(string(b)))
			if fi, _ := f.Info(); fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v", f.Name(), fi.Mode().Perm())
			}
		}
	}
	if len(seeds) < 3 {
		t.Fatalf("expected session + 2 service key files, got %d", len(seeds))
	}
	for _, name := range []string{"keys.json", "jwks.json", "state.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, seed := range seeds {
			if strings.Contains(string(b), seed) {
				t.Errorf("%s holds private key material", name)
			}
		}
		if fi, _ := os.Stat(filepath.Join(dir, name)); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", name, fi.Mode().Perm())
		}
	}
	for _, seed := range seeds {
		if strings.Contains(logs.String(), seed) {
			t.Error("a private seed reached the log")
		}
	}
}

func jwksKIDs(t *testing.T, is *Issuer) []string {
	t.Helper()
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(is.Keys.JWKS(), &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, k := range doc.Keys {
		out = append(out, k.Kid)
	}
	return out
}

// Consumers built on go-auth REPLACE their key set on every JWKS refresh, so a
// ten-year service token works only if /.well-known/jwks.json keeps publishing
// its key for as long as a live record needs it, and the active service key
// always, and stops publishing a rotated-out one once nothing needs it.
func TestJWKSPublishesExactlyTheServiceKeysLiveTokensNeed(t *testing.T) {
	is, _ := newIssuer(t)
	_, recA, _ := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
	// (1) the active svc- key is published, with the session key.
	if got := jwksKIDs(t, is); !containsKID(got, recA.KID) || len(got) != 2 {
		t.Fatalf("jwks %v, want the session key and %s", got, recA.KID)
	}
	// Rotate: the new active key is published AND the rotated-out one, still named by a live record.
	res, err := is.RotateServiceKey(operatorP, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := jwksKIDs(t, is); !containsKID(got, res.NewKID) || !containsKID(got, recA.KID) || len(got) != 3 {
		t.Fatalf("after rotate: %v", got)
	}
	// An hourly Maintain pass in the meantime changes nothing while the record is live.
	if _, err := is.Keys.PruneWith(is.ServiceKeyInUse); err != nil {
		t.Fatal(err)
	}
	if got := jwksKIDs(t, is); !containsKID(got, recA.KID) {
		t.Fatalf("a prune dropped a key a live token needs: %v", got)
	}
	// (2) once the last record on the rotated-out key is revoked it drops out of the JWKS at once.
	if _, err := is.RevokeService(recA.JTI); err != nil {
		t.Fatal(err)
	}
	if got := jwksKIDs(t, is); containsKID(got, recA.KID) || !containsKID(got, res.NewKID) || len(got) != 2 {
		t.Fatalf("after the last record on the old key was revoked: %v", got)
	}
	// ...and an EXPIRED record frees its key at the next Maintain pass.
	_, recB, _ := is.IssueService(operatorP, ServiceRequest{App: "nanite", Audience: "tether", Scopes: []string{"events"}})
	res2, _ := is.RotateServiceKey(operatorP, false)
	if got := jwksKIDs(t, is); !containsKID(got, recB.KID) {
		t.Fatalf("%v", got)
	}
	is.Now = func() time.Time { return time.Now().Add(ServiceTokenLifetime + time.Hour) }
	is.State.now = is.Now
	if _, err := is.Keys.PruneWith(is.ServiceKeyInUse); err != nil {
		t.Fatal(err)
	}
	if got := jwksKIDs(t, is); containsKID(got, recB.KID) || !containsKID(got, res2.NewKID) {
		t.Fatalf("after expiry: %v", got)
	}
}

// (3) The real library verifier, as every external app runs it, refreshing its
// JWKS after a session-key rotation + prune, still verifies a service token.
func TestExternalGoAuthVerifierStillVerifiesAServiceTokenAfterRotationAndPrune(t *testing.T) {
	is, _ := newIssuer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(is.Keys.JWKS())
	}))
	t.Cleanup(srv.Close)
	svc, svcRec, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := is.Exchange(operatorP, Request{Audience: "tether", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	jv, err := auth.NewJWTVerifier(auth.JWTConfig{Issuer: is.Name, Audience: "tether", JWKSURL: srv.URL, MinRefresh: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := jv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for name, tk := range map[string]string{"service": svc.Token, "session": sess.Token} {
		if _, err := jv.Verify(ctx, tk); err != nil {
			t.Fatalf("%s token before rotation: %v", name, err)
		}
	}
	// Ordinary key rotation, then past the overlap and the hourly prune.
	if err := is.Rotate(); err != nil {
		t.Fatal(err)
	}
	is.Keys.now = func() time.Time { return time.Now().Add(DefaultKeyOverlap + time.Hour) }
	is.Now = is.Keys.now
	if _, err := is.Keys.PruneWith(is.ServiceKeyInUse); err != nil {
		t.Fatal(err)
	}
	// The consumer replaces its key set on refresh (the library does not merge).
	if err := jv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if p, err := jv.Verify(ctx, svc.Token); err != nil || p.Kind != "service" || p.TokenID != svcRec.JTI {
		t.Fatalf("service token after rotation + prune + refresh: %+v %v", p, err)
	}
	if _, err := jv.Verify(ctx, sess.Token); err == nil {
		t.Fatal("the pruned session key's token still verifies on the consumer")
	}
	// A service-key rotation: the old token keeps verifying on the consumer after its refresh.
	res, err := is.RotateServiceKey(operatorP, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := jv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := jv.Verify(ctx, svc.Token); err != nil {
		t.Fatalf("old service token after a service key rotation: %v", err)
	}
	if _, err := jv.Verify(ctx, res.Reissued[0].Token.Token); err != nil {
		t.Fatalf("replacement token: %v", err)
	}
	// After the old token is revoked its key is gone from the JWKS; the consumer's next refresh drops it.
	if _, err := is.RevokeService(svcRec.JTI); err != nil {
		t.Fatal(err)
	}
	if err := jv.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := jv.Verify(ctx, svc.Token); err == nil {
		t.Fatal("revoked token still verifies on a consumer with no deny-list")
	}
	if _, err := jv.Verify(ctx, res.Reissued[0].Token.Token); err != nil {
		t.Fatalf("replacement after the old key was dropped: %v", err)
	}
}

// An earlier denial of the same jti (an admin's, for the normal horizon) must not
// cut the revocation short: the entry is extended to the token's exp, survives a
// restart, and a longer entry is never shortened.
func TestRevokeServiceExtendsAnEarlierShorterDenial(t *testing.T) {
	is, dir := newIssuer(t)
	_, rec, err := is.IssueService(operatorP, ServiceRequest{App: "cerberus", Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := is.State.Deny(DenyJTI, rec.JTI, time.Now().Add(DenyHorizon)); err != nil {
		t.Fatal(err)
	}
	if _, err := is.RevokeService(rec.JTI); err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return time.Now().Add(DenyHorizon + 24*time.Hour) }
	is.State.now = later
	if !is.State.Denied(DenyJTI, rec.JTI) {
		t.Fatal("the revoked token verifies again after the earlier denial's horizon")
	}
	st2, err := OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st2.now = later
	if !st2.Denied(DenyJTI, rec.JTI) {
		t.Fatal("the extended denial was lost on restart")
	}

	// A denial that already outlasts the token is not shortened.
	is2, _ := newIssuer(t)
	_, rec2, err := is2.IssueService(operatorP, ServiceRequest{App: "nanite", Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	long := rec2.ExpiresAt.Add(24 * time.Hour)
	if err := is2.State.Deny(DenyJTI, rec2.JTI, long); err != nil {
		t.Fatal(err)
	}
	if _, err := is2.RevokeService(rec2.JTI); err != nil {
		t.Fatal(err)
	}
	for _, e := range is2.State.doc.Deny {
		if e.Kind == DenyJTI && e.Value == rec2.JTI && !e.Until.Equal(long) {
			t.Fatalf("a longer denial was shortened to %v", e.Until)
		}
	}
}
