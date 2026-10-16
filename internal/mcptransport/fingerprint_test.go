package mcptransport

import (
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

func issuedCaller() Caller {
	return Caller{
		Principal: identity.Principal{Verified: true, ID: "msg://session/local/s1", Kind: "agent", SessionID: "s1", Issuer: "tether", Audience: "tether",
			TokenID: "jti-1", Parent: "msg://device/w1", Scopes: []string{"tether.read", "tether.operate"}},
		Policy: mcpgateway.SessionPolicy{SessionID: "s1", AgentID: "agent", Servers: []string{}, Digest: "policy-digest"},
	}
}

func fingerprint(t *testing.T, c Caller, token string) string {
	t.Helper()
	fp, err := viewFingerprint(c, token, mcpadapter.ProxyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// CW-20261016-0160 (architect ruling mcp_session_fingerprint_issued_tokens_2026_10_16): an issued JWT is refreshed in
// place within one MCP session, so its fingerprint is the authority it carries, never the token.
func TestViewFingerprintIssuedTokensPinAuthorityNotTheToken(t *testing.T) {
	base := issuedCaller()
	want := fingerprint(t, base, "jwt-one")

	// A refresh: new token string, new jti, same authority. Scope order is irrelevant.
	refreshed := issuedCaller()
	refreshed.Principal.TokenID = "jti-2"
	refreshed.Principal.Scopes = []string{"tether.operate", "tether.read"}
	if got := fingerprint(t, refreshed, "jwt-two-a-completely-different-string"); got != want {
		t.Fatal("a refreshed issued token (new token, new jti) changed the fingerprint")
	}

	// Any change of authority changes it, narrowing included.
	for name, mutate := range map[string]func(*Caller){
		"narrowed scopes": func(c *Caller) { c.Principal.Scopes = []string{"tether.read"} },
		"widened scopes":  func(c *Caller) { c.Principal.Scopes = append(c.Principal.Scopes, "tether.maintain") },
		"other session":   func(c *Caller) { c.Principal.SessionID = "s2" },
		"other principal": func(c *Caller) { c.Principal.ID = "msg://session/local/s2" },
		"other kind":      func(c *Caller) { c.Principal.Kind = "session" },
		"other audience":  func(c *Caller) { c.Principal.Audience = "tesseract" },
		"other parent":    func(c *Caller) { c.Principal.Parent = "msg://device/w2" },
		"other issuer":    func(c *Caller) { c.Principal.Issuer = "someone-else" },
		"other policy":    func(c *Caller) { c.Policy.Digest = "another-digest" },
	} {
		c := issuedCaller()
		mutate(&c)
		if fingerprint(t, c, "jwt-one") == want {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}
}

// Stored credentials keep pinning the token itself, and never collide with an issued fingerprint.
func TestViewFingerprintStoredCredentialsStillPinTheToken(t *testing.T) {
	stored := Caller{Principal: identity.Principal{Verified: true, ID: "same-principal", Kind: "service", Scopes: []string{"tether.read"}},
		Policy: mcpgateway.SessionPolicy{Digest: "d"}}
	one, two := fingerprint(t, stored, "tth_one"), fingerprint(t, stored, "tth_two")
	if one == two {
		t.Fatal("two stored credentials of one principal share a fingerprint")
	}
	if one != fingerprint(t, stored, "tth_one") {
		t.Fatal("the same stored credential is not stable")
	}
	issued := issuedCaller()
	issued.Principal.ID, issued.Principal.Kind, issued.Principal.SessionID = "same-principal", "service", ""
	issued.Principal.Scopes, issued.Policy.Digest = []string{"tether.read"}, "d"
	if fingerprint(t, issued, "tth_one") == one {
		t.Fatal("an issued token and a stored credential produced the same fingerprint")
	}
}
