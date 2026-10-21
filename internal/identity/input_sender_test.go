package identity

import "testing"

// CW-20261021-0010 policy: operator only for a verified interactive caller; any
// doubt is unknown, never operator.
func TestClassifyInputSender(t *testing.T) {
	op := Principal{ID: OperatorID, Kind: "operator", Verified: true}
	for name, tc := range map[string]struct {
		p     Principal
		ok    bool
		class string
	}{
		"local operator credential (shared)":         {op, true, InputClassUnknown},
		"tachyon device operate+maintain+read":       {Principal{ID: "msg://device/t", Kind: "device", Verified: true, Scopes: []string{"maintain", "operate", "read"}}, true, InputClassOperator},
		"device with canonical tether.operate":       {Principal{ID: "msg://device/t", Kind: "device", Verified: true, Scopes: []string{"tether.operate"}}, true, InputClassOperator},
		"worker-only device (host credential)":       {Principal{ID: "msg://device/w", Kind: "device", Verified: true, Scopes: []string{"worker"}}, true, InputClassSystem},
		"device with worker AND operate":             {Principal{ID: "msg://device/h", Kind: "device", Verified: true, Scopes: []string{"operate", "read", "worker"}}, true, InputClassOperator},
		"read-only device":                           {Principal{ID: "msg://device/r", Kind: "device", Verified: true, Scopes: []string{"read"}}, true, InputClassUnknown},
		"admin-only device":                          {Principal{ID: "msg://device/a", Kind: "device", Verified: true, Scopes: []string{"admin"}}, true, InputClassUnknown},
		"session principal":                          {Principal{ID: "msg://session/local/x", Kind: "session", Verified: true}, true, InputClassAgent},
		"agent principal":                            {Principal{ID: "msg://agent/a/b", Kind: "agent", Verified: true}, true, InputClassAgent},
		"service principal":                          {Principal{ID: "msg://service/cerberus/dev", Kind: "service", Verified: true}, true, InputClassSystem},
		"unknown kind":                               {Principal{ID: "msg://x/y", Kind: "robot", Verified: true}, true, InputClassUnknown},
		"no principal in the context":                {Principal{}, false, InputClassUnknown},
		"unverified principal with operate":          {Principal{ID: "msg://device/t", Kind: "device", Verified: false, Scopes: []string{"operate"}}, true, InputClassUnknown},
		"unverified operator-kind principal":         {Principal{ID: OperatorID, Kind: "operator", Verified: false}, true, InputClassUnknown},
		"operator-minted narrow token is not the op": {Principal{ID: OperatorID, Kind: "operator", Verified: true, TokenID: "jti"}, true, InputClassUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			_, class, _ := ClassifyInputSender(tc.p, tc.ok)
			if class != tc.class {
				t.Fatalf("class %q, want %q", class, tc.class)
			}
		})
	}
}

// Negative sweep (mutation guard): across every kind and scope set, an
// UNVERIFIED principal, or none, can never be classified operator.
func TestUnverifiedSenderIsNeverOperator(t *testing.T) {
	kinds := []string{"operator", "device", "session", "agent", "service", "user", ""}
	scopes := [][]string{nil, {"operate"}, {"tether.operate"}, {"*"}, {"operate", "maintain", "read"}, {"worker"}}
	for _, kind := range kinds {
		for _, sc := range scopes {
			p := Principal{ID: OperatorID, Kind: kind, Verified: false, Scopes: sc}
			if _, class, _ := ClassifyInputSender(p, true); class == InputClassOperator {
				t.Fatalf("unverified kind=%q scopes=%v classified operator", kind, sc)
			}
			p.Verified = true
			if _, class, _ := ClassifyInputSender(p, false); class == InputClassOperator {
				t.Fatalf("a principal not present in the context classified operator (kind=%q)", kind)
			}
		}
	}
}

// The shared operator credential is held by a human at the CLI and by local agents
// alike, so across every scope set it is never operator and says why.
func TestSharedOperatorCredentialIsNeverOperator(t *testing.T) {
	for _, sc := range [][]string{nil, {"operate"}, {"tether.operate"}, {"*"}, {"operate", "maintain", "read"}, {"worker"}} {
		p := Principal{ID: OperatorID, Kind: "operator", Verified: true, Scopes: sc}
		_, class, basis := ClassifyInputSender(p, true)
		if class != InputClassUnknown || basis != InputBasisSharedOperator {
			t.Fatalf("operator credential scopes=%v: class %q basis %q", sc, class, basis)
		}
	}
}
