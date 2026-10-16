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
