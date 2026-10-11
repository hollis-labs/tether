package identity

import (
	"context"
	"testing"
)

// TestVerifiedFlagByPrincipalKind pins the single definition (RK-044): any
// principal that reaches the context through a verified bearer credential is
// verified, whatever its kind; anything else, including a Principal that claims
// Verified itself, is not.
func TestVerifiedFlagByPrincipalKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *Principal
		want bool
	}{
		{"operator", &Principal{ID: OperatorID, Kind: "operator"}, true},
		{"session", &Principal{ID: "session:s", Kind: "session", SessionID: "s"}, true},
		{"device", &Principal{ID: "device:d", Kind: "device"}, true},
		{"service", &Principal{ID: "svc", Kind: "service"}, true},
		{"anonymous", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.p != nil {
				ctx = WithPrincipal(ctx, *tc.p)
			}
			if got := Verified(ctx); got != tc.want {
				t.Fatalf("Verified = %v, want %v", got, tc.want)
			}
			pp := PolicyPrincipalFrom(ctx)
			if pp.Verified != tc.want || (tc.p != nil) != (pp.ID != "") || (tc.p != nil && (pp.ID != tc.p.ID || pp.Kind != tc.p.Kind)) {
				t.Fatalf("policy principal = %+v", pp)
			}
			if p, ok := FromContext(ctx); ok != (tc.p != nil) || p.Verified != tc.want {
				t.Fatalf("FromContext = %+v, %v", p, ok)
			}
		})
	}
	if Verified(context.WithValue(context.Background(), principalKey{}, Principal{ID: "forged"})) {
		t.Fatal("principal not placed by WithPrincipal must not be verified")
	}
}
