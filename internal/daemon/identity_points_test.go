package daemon

import (
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestListenerBindUsesEffectivePointModes(t *testing.T) {
	pub := "tcp:0.0.0.0:8787"
	cases := []struct {
		name   string
		cfg    Config
		mcp    bool
		wantOK bool
	}{
		{"global enforce, no overrides", Config{ListenAddr: pub, IdentityMode: identity.Enforce}, true, true},
		{"global enforce, local_api observe", Config{ListenAddr: pub, IdentityMode: identity.Enforce, IdentityPoints: map[string]identity.Mode{"local_api": identity.Observe}}, false, false},
		{"global enforce, local_api off", Config{ListenAddr: pub, IdentityMode: identity.Enforce, IdentityPoints: map[string]identity.Mode{"local_api": identity.Off}}, false, false},
		{"global enforce, mcp_gateway observe, mcp mounted", Config{ListenAddr: pub, IdentityMode: identity.Enforce, IdentityPoints: map[string]identity.Mode{"mcp_gateway": identity.Observe}}, true, false},
		{"global enforce, mcp_gateway observe, mcp absent", Config{ListenAddr: pub, IdentityMode: identity.Enforce, IdentityPoints: map[string]identity.Mode{"mcp_gateway": identity.Observe}}, false, true},
		{"global observe, local_api enforce", Config{ListenAddr: pub, IdentityMode: identity.Observe, IdentityPoints: map[string]identity.Mode{"local_api": identity.Enforce}}, false, true},
		{"loopback unaffected", Config{ListenAddr: "tcp:127.0.0.1:8787", IdentityMode: identity.Observe}, true, true},
	}
	for _, c := range cases {
		if err := c.cfg.ValidateListenerBind(c.mcp); (err == nil) != c.wantOK {
			t.Errorf("%s: err=%v wantOK=%v", c.name, err, c.wantOK)
		}
	}
	if got := (Config{IdentityMode: identity.Off, IdentityPoints: map[string]identity.Mode{"teams": identity.Enforce}}).StrictestIdentityMode(); got != identity.Enforce {
		t.Fatalf("strictest = %s", got)
	}
}
