package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

// The launch-time app stamp (CW-20261020-0036): only the verified local operator sets it;
// an agent or session is refused; anyone else has the field dropped and audited.
func TestAppStampPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      *identity.Principal
		app    string
		want   string
		status int
		audit  bool
	}{
		{"operator, valid app", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, "torque", "torque", http.StatusOK, false},
		{"operator, no app", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, "", "", http.StatusOK, false},
		{"operator, valid shape but not a registered producer", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, "apiarcscratch", "", http.StatusBadRequest, false},
		{"operator, uppercase", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, "Torque", "", http.StatusBadRequest, false},
		{"operator, path-like", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, "a/b", "", http.StatusBadRequest, false},
		{"operator, too long", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, strings.Repeat("a", 33), "", http.StatusBadRequest, false},
		{"agent", &identity.Principal{Verified: true, ID: "agent:a", Kind: "agent"}, "torque", "", http.StatusForbidden, false},
		{"session credential", &identity.Principal{Verified: true, ID: "session:s", Kind: "session", SessionID: "s"}, "torque", "", http.StatusForbidden, false},
		{"worker device", &identity.Principal{Verified: true, ID: "msg://device/w", Kind: "device"}, "torque", "", http.StatusOK, true},
		{"service", &identity.Principal{Verified: true, ID: "msg://service/torque/dev", Kind: "service", TokenID: "j"}, "torque", "", http.StatusOK, true},
		{"operator-minted narrow token", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "jti-1"}, "torque", "", http.StatusOK, true},
		{"unknown future kind", &identity.Principal{Verified: true, ID: "x", Kind: "brand-new-kind"}, "torque", "", http.StatusOK, true},
		{"no principal", nil, "torque", "", http.StatusOK, false},
	} {
		var audited []identity.Observation
		s := &Server{IdentityAudit: func(_ context.Context, o identity.Observation) error {
			audited = append(audited, o)
			return nil
		}}
		r := httptest.NewRequest("POST", "/sessions", nil)
		if tc.p != nil {
			r = r.WithContext(identity.WithPrincipal(r.Context(), *tc.p))
		}
		w := httptest.NewRecorder()
		got, ok := s.appStamp(w, r, tc.app)
		status := http.StatusOK
		if !ok {
			status = w.Code
		}
		if status != tc.status || got != tc.want {
			t.Errorf("%s: stamp %q status %d, want %q / %d", tc.name, got, status, tc.want, tc.status)
		}
		if (len(audited) == 1 && audited[0].Event == eventAppDropped) != tc.audit || len(audited) > 1 {
			t.Errorf("%s: audit rows %+v, want drop audited=%v", tc.name, audited, tc.audit)
		}
	}
}
