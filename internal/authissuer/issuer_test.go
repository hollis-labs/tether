package authissuer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

type fakeStored struct{ p map[string]identity.Principal }

func (f fakeStored) Verify(_ context.Context, token string) (identity.Principal, error) {
	if p, ok := f.p[token]; ok {
		p.Verified = true
		return p, nil
	}
	return identity.Principal{}, identity.ErrInvalidToken
}

func newIssuer(t *testing.T) (*Issuer, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "auth")
	ks, err := OpenKeyStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return New("tether-test", ks, st), dir
}

func claimsOf(t *testing.T, tok string) Claims {
	t.Helper()
	parts := strings.Split(tok, ".")
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c Claims
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestKeyFilesAre0600AndRotationKeepsOverlap(t *testing.T) {
	is, dir := newIssuer(t)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		fi, _ := e.Info()
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", e.Name(), fi.Mode().Perm())
		}
	}
	old := is.Keys.KIDs()[0]
	if err := is.Rotate(); err != nil {
		t.Fatal(err)
	}
	kids := is.Keys.KIDs()
	if len(kids) != 2 || kids[0] == old {
		t.Fatalf("kids %v old %s", kids, old)
	}
	if !strings.Contains(string(is.Keys.JWKS()), old) {
		t.Fatal("rotated-out key must stay in the JWKS during overlap")
	}
	// after the overlap window the old key is gone
	now := time.Now().Add(2 * time.Hour)
	is.Keys.now = func() time.Time { return now }
	if removed, err := is.Keys.Prune(); err != nil || !removed {
		t.Fatalf("prune: %v %v", removed, err)
	}
	if strings.Contains(string(is.Keys.JWKS()), old) {
		t.Fatal("old key still published after overlap")
	}
	if _, err := os.Stat(filepath.Join(dir, old+".key")); !os.IsNotExist(err) {
		t.Fatal("old private key not deleted")
	}
	// reopening loads the same keys; a loose key file is refused
	again, err := OpenKeyStore(dir, time.Hour)
	if err != nil || again.KIDs()[0] != is.Keys.KIDs()[0] {
		t.Fatalf("reopen: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, is.Keys.KIDs()[0]+".key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenKeyStore(dir, time.Hour); err == nil {
		t.Fatal("loose key mode accepted")
	}
}

func TestExchangeRules(t *testing.T) {
	is, _ := newIssuer(t)
	device := identity.Principal{Verified: true, ID: "msg://device/d1", Kind: "device", Scopes: []string{"read", "operate"}}
	operator := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	cases := []struct {
		name    string
		parent  identity.Principal
		req     Request
		wantErr error
	}{
		{"device subset", device, Request{Audience: "tether", Scopes: []string{"read"}}, nil},
		{"device bare+canonical", device, Request{Audience: "tether", Scopes: []string{"tether.operate", "read"}}, nil},
		{"device exceeds", device, Request{Audience: "tether", Scopes: []string{"admin"}}, ErrForbidden},
		{"device other app", device, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}}, ErrForbidden},
		{"operator other app", operator, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}}, nil},
		{"scope for wrong app", operator, Request{Audience: "tesseract", Scopes: []string{"torque.read"}}, ErrInvalid},
		{"wildcard refused", operator, Request{Audience: "tesseract", Scopes: []string{"tesseract.*"}}, ErrInvalid},
		{"bad audience", operator, Request{Audience: "a b", Scopes: []string{"tesseract.read"}}, ErrInvalid},
		{"no scopes", operator, Request{Audience: "tesseract"}, ErrInvalid},
		{"ttl too long", operator, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}, TTL: 61 * time.Minute}, ErrInvalid},
		{"ttl max ok", operator, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}, TTL: 60 * time.Minute}, nil},
		{"agent parent", identity.Principal{Verified: true, ID: "x", Kind: "agent", Scopes: []string{"*"}}, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}}, ErrForbidden},
		{"issued token parent", identity.Principal{Verified: true, ID: "x", Kind: "device", TokenID: "j", Scopes: []string{"*"}}, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}}, ErrForbidden},
		{"session credential parent", identity.Principal{Verified: true, ID: "s", Kind: "session", Scopes: []string{"*"}}, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}}, ErrForbidden},
		{"unverified", identity.Principal{ID: "x", Kind: "device", Scopes: []string{"*"}}, Request{Audience: "tether", Scopes: []string{"read"}}, ErrForbidden},
	}
	for _, c := range cases {
		_, err := is.Exchange(c.parent, c.req)
		if c.wantErr == nil && err != nil || c.wantErr != nil && !errors.Is(err, c.wantErr) {
			t.Errorf("%s: err=%v want %v", c.name, err, c.wantErr)
		}
	}
	tok, err := is.Exchange(operator, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}})
	if err != nil {
		t.Fatal(err)
	}
	c := claimsOf(t, tok.Token)
	if c.Aud != "tesseract" || c.Iss != "tether-test" || c.Kind != "operator" || c.JTI == "" || c.Par != identity.OperatorID || tok.TTL != 900 {
		t.Fatalf("claims %+v ttl %d", c, tok.TTL)
	}
}

func TestSessionGrantBoundsDeviceAndRevocationStopsMinting(t *testing.T) {
	is, _ := newIssuer(t)
	device := identity.Principal{Verified: true, ID: "msg://device/d1", Kind: "device", Scopes: []string{"worker"}}
	other := identity.Principal{Verified: true, ID: "msg://device/d2", Kind: "device", Scopes: []string{"worker"}}
	req := Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}, SessionID: "s1"}
	if _, err := is.Exchange(device, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no grant must refuse: %v", err)
	}
	if err := is.RegisterSessionGrant("s1", device.ID, map[string][]string{"tesseract": {"tesseract.read"}, "tether": {"read"}}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	tok, err := is.Exchange(device, req)
	if err != nil {
		t.Fatal(err)
	}
	if c := claimsOf(t, tok.Token); c.Kind != "agent" || c.SID != "s1" || c.Sub != SessionPrincipalID("s1") || c.Par != device.ID {
		t.Fatalf("claims %+v", c)
	}
	if _, err := is.Exchange(other, req); !errors.Is(err, ErrForbidden) {
		t.Fatal("another device used the grant")
	}
	if _, err := is.Exchange(device, Request{Audience: "tesseract", Scopes: []string{"tesseract.write"}, SessionID: "s1"}); !errors.Is(err, ErrForbidden) {
		t.Fatal("scope beyond the grant issued")
	}
	if err := is.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := is.Exchange(device, req); !errors.Is(err, ErrForbidden) {
		t.Fatal("minting continued after the session ended")
	}
	body, etag := is.State.DenyDoc()
	if !strings.Contains(string(body), `"sid":["s1"]`) || etag == "" {
		t.Fatalf("deny doc %s %s", body, etag)
	}
	// persisted across a restart
	again, err := OpenState(is.State.path)
	if err != nil || !again.Denied(DenySID, "s1") {
		t.Fatalf("deny-list not persisted: %v", err)
	}
}

func TestLocalVerifierAcceptsIssuedAndStoredAndHonorsDenyList(t *testing.T) {
	is, _ := newIssuer(t)
	stored := fakeStored{map[string]identity.Principal{"tth_x": {ID: "msg://device/d1", Kind: "device", Scopes: []string{"read"}}}}
	v, err := NewVerifier(stored, is)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if p, err := v.Verify(ctx, "tth_x"); err != nil || p.ID != "msg://device/d1" || p.TokenID != "" {
		t.Fatalf("stored: %+v %v", p, err)
	}
	operator := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	tok, _ := is.Exchange(operator, Request{Audience: "tether", Scopes: []string{"read"}, SessionID: "s9"})
	p, err := v.Verify(ctx, tok.Token)
	if err != nil || p.Kind != "agent" || p.SessionID != "s9" || p.TokenID == "" || !identity.ScopeGranted(p.Scopes, "read") || identity.ScopeGranted(p.Scopes, "operate") {
		t.Fatalf("issued: %+v %v", p, err)
	}
	if p.Issuer != is.Name || p.Audience != TetherAudience || p.Parent != identity.OperatorID {
		t.Fatalf("issued principal lacks its token authority (iss/aud/par): %+v", p)
	}
	// a token for another audience is not accepted by Tether's own API
	other, _ := is.Exchange(operator, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}})
	if _, err := v.Verify(ctx, other.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("wrong audience: %v", err)
	}
	_ = is.RevokeSession("s9")
	if _, err := v.Verify(ctx, tok.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("revoked sid still verifies: %v", err)
	}
	// rotation is picked up by the in-process verifier through the JWKS refetch
	_ = is.Rotate()
	fresh, _ := is.Exchange(operator, Request{Audience: "tether", Scopes: []string{"read"}})
	time.Sleep(1100 * time.Millisecond) // MinRefresh
	if _, err := v.Verify(ctx, fresh.Token); err != nil {
		t.Fatalf("token from rotated key: %v", err)
	}
}

func TestRotateKeepsAnActiveKeyWhenGenerationFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	is, dir := newIssuer(t)
	before, _ := is.Keys.Active()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := is.Rotate(); err == nil {
		t.Fatal("rotate succeeded in an unwritable key dir")
	}
	if kid, priv := is.Keys.Active(); kid != before || priv == nil {
		t.Fatalf("active key lost after failed rotate: %q", kid)
	}
}

func TestServiceSessionMintNeedsLaunchGrantAndIsCapped(t *testing.T) {
	is, _ := newIssuer(t)
	svc := identity.Principal{Verified: true, ID: "msg://service/launcher", Kind: "service", Scopes: []string{"*"}}
	other := identity.Principal{Verified: true, ID: "msg://service/other", Kind: "service", Scopes: []string{"*"}}
	req := Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}, SessionID: "s1"}
	if _, err := is.Exchange(svc, req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("service without a grant minted for a session: %v", err)
	}
	// non-sid service tokens are unchanged
	if _, err := is.Exchange(svc, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}}); err != nil {
		t.Fatalf("non-sid service mint: %v", err)
	}
	if err := is.RegisterSessionGrant("s1", svc.ID, map[string][]string{"tesseract": {"tesseract.read"}}, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	tok, err := is.Exchange(svc, req)
	if err != nil {
		t.Fatal(err)
	}
	if c := claimsOf(t, tok.Token); c.Kind != "agent" || c.SID != "s1" || c.Par != svc.ID {
		t.Fatalf("claims %+v", c)
	}
	if tok.ExpiresAt.After(time.Now().Add(11 * time.Minute)) {
		t.Fatalf("expiry not capped by grant: %v", tok.ExpiresAt)
	}
	if _, err := is.Exchange(other, req); !errors.Is(err, ErrForbidden) {
		t.Fatal("another service used the grant")
	}
	if _, err := is.Exchange(svc, Request{Audience: "tesseract", Scopes: []string{"tesseract.write"}, SessionID: "s1"}); !errors.Is(err, ErrForbidden) {
		t.Fatal("scope beyond the grant issued")
	}
	// the operator stays unrestricted
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	if _, err := is.Exchange(op, Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}, SessionID: "s2"}); err != nil {
		t.Fatalf("operator sid mint: %v", err)
	}
}

func TestOperatorRotationDeniesAlreadyIssuedTokens(t *testing.T) {
	is, dir := newIssuer(t)
	stored := fakeStored{map[string]identity.Principal{}}
	v, err := NewVerifier(stored, is)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	operator := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	device := identity.Principal{Verified: true, ID: "msg://device/d1", Kind: "device", Scopes: []string{"read"}}
	fromOperator, _ := is.Exchange(operator, Request{Audience: "tether", Scopes: []string{"read"}})
	sidFromOperator, _ := is.Exchange(operator, Request{Audience: "tether", Scopes: []string{"read"}, SessionID: "s1"})
	fromDevice, _ := is.Exchange(device, Request{Audience: "tether", Scopes: []string{"read"}})
	for _, tok := range []Token{fromOperator, sidFromOperator, fromDevice} {
		if _, err := v.Verify(ctx, tok.Token); err != nil {
			t.Fatalf("token should verify before rotation: %v", err)
		}
	}
	// Tracking survives a daemon restart (state.json), so a rotation after a
	// restart still denies what was minted before it.
	st2, err := OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := st2.DenyMintedBy(identity.OperatorID)
	if err != nil || n != 2 {
		t.Fatalf("denied %d tokens (%v), want the 2 operator-minted ones", n, err)
	}
	v2, err := NewVerifier(stored, New(is.Name, is.Keys, st2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v2.Verify(ctx, fromOperator.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("operator-minted token survived rotation: %v", err)
	}
	if _, err := v2.Verify(ctx, sidFromOperator.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("operator-minted session token survived rotation: %v", err)
	}
	if _, err := v2.Verify(ctx, fromDevice.Token); err != nil {
		t.Fatalf("a device-minted token must be unaffected: %v", err)
	}
	// the denied jtis are on the wire deny-list that other apps poll
	body, _ := st2.DenyDoc()
	if !strings.Contains(string(body), claimsOf(t, fromOperator.Token).JTI) {
		t.Fatal("denied jti missing from the deny-list document")
	}
	// idempotent, and a token minted afterwards (by the new credential) is fine
	if n, err := st2.DenyMintedBy(identity.OperatorID); err != nil || n != 0 {
		t.Fatalf("second run denied %d (%v)", n, err)
	}
	fresh, _ := (New(is.Name, is.Keys, st2)).Exchange(operator, Request{Audience: "tether", Scopes: []string{"read"}})
	if _, err := v2.Verify(ctx, fresh.Token); err != nil {
		t.Fatalf("token minted after the rotation was denied: %v", err)
	}
}

func TestOperatorMintFailsClosedWhenItCannotBeTracked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	is, dir := newIssuer(t)
	operator := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := is.Exchange(operator, Request{Audience: "tether", Scopes: []string{"read"}}); err == nil {
		t.Fatal("minted an untrackable operator token")
	}
}

// identity.IsBoundAgentToken must recognize exactly what the issuer mints.
func TestIssuedSessionTokenIsABoundAgentToken(t *testing.T) {
	is, _ := newIssuer(t)
	v, err := NewVerifier(fakeStored{map[string]identity.Principal{}}, is)
	if err != nil {
		t.Fatal(err)
	}
	if SessionPrincipalID("s7") != identity.SessionPrincipalIDPrefix+"s7" {
		t.Fatal("SessionPrincipalID drifted from identity.SessionPrincipalIDPrefix")
	}
	operator := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	withSid, _ := is.Exchange(operator, Request{Audience: "tether", Scopes: []string{"tether.read"}, SessionID: "s7"})
	noSid, _ := is.Exchange(operator, Request{Audience: "tether", Scopes: []string{"tether.read"}})
	p, err := v.Verify(context.Background(), withSid.Token)
	if err != nil || !identity.IsBoundAgentToken(p) {
		t.Fatalf("minted session token is not a bound agent token: %+v %v", p, err)
	}
	q, err := v.Verify(context.Background(), noSid.Token)
	if err != nil || identity.IsBoundAgentToken(q) {
		t.Fatalf("operator-minted narrow token passed as a bound agent token: %+v %v", q, err)
	}
}

// Architect note on A1: the composite verifier dispatches on the credential's FORMAT with no fallthrough. A JWT-shaped
// value is judged by issuer verification only, even if the stored store would accept the same string; a stored
// credential never reaches the JWT verifier. A failure on one path is final.
func TestCompositeVerifierDispatchesOnFormatWithoutFallthrough(t *testing.T) {
	is, _ := newIssuer(t)
	const jwtShaped = "aaa.bbb.ccc"
	calls := 0
	stored := countingStored{inner: fakeStored{map[string]identity.Principal{
		"tth_good":  {ID: "msg://device/d1", Kind: "device"},
		jwtShaped:   {ID: "msg://device/sneaky", Kind: "device"},
		"plain-tok": {ID: "msg://device/plain", Kind: "device"},
	}}, calls: &calls}
	v, err := NewVerifier(stored, is)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A JWT-shaped string that the stored store would accept is still refused: the stored store is never asked.
	if _, err := v.Verify(ctx, jwtShaped); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("JWT-shaped credential fell through to the stored store: %v", err)
	}
	if calls != 0 {
		t.Fatalf("the stored store was consulted %d times for a JWT-shaped credential", calls)
	}
	// Stored formats go to the stored store.
	for _, tok := range []string{"tth_good", "plain-tok"} {
		if p, err := v.Verify(ctx, tok); err != nil || p.TokenID != "" {
			t.Fatalf("%s: %+v %v", tok, p, err)
		}
	}
	if calls != 2 {
		t.Fatalf("stored store calls = %d, want 2", calls)
	}
	// A tth_ credential is never handed to the JWT verifier, even when it has two dots.
	if _, err := v.Verify(ctx, "tth_a.b.c"); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("tth_ with dots: %v", err)
	}
	if calls != 3 {
		t.Fatalf("tth_ with dots must be judged by the stored store only (calls = %d)", calls)
	}
}

type countingStored struct {
	inner fakeStored
	calls *int
}

func (c countingStored) Verify(ctx context.Context, token string) (identity.Principal, error) {
	*c.calls++
	return c.inner.Verify(ctx, token)
}
