package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

// eventAppDropped is the identity_audit event for an app a non-operator sent and the
// launch discarded.
const eventAppDropped = "app_dropped"

// appStamp decides what a launch request's app does (CW-20261020-0036). The app is the
// one the session may later request a service token for, so it is stamped by the hub
// at launch and only an explicit allow may set it:
//
//   - the verified local operator: accepted when it is a valid service app id (400
//     otherwise);
//   - an agent or a session credential: refused with 403 app_not_permitted;
//   - any other caller: the field is dropped, the request proceeds with no app, and
//     the drop is recorded in identity_audit.
//
// A trusted launcher with its own service identity may later set only its own app
// (CW-20261019-0047); until then the operator credential is the root of trust.
func (s *Server) appStamp(w http.ResponseWriter, r *http.Request, app string) (string, bool) {
	if app == "" {
		return "", true
	}
	p, ok := identity.FromContext(r.Context())
	if ok && (p.Kind == "session" || p.Kind == "agent" || p.SessionID != "") {
		writeError(w, http.StatusForbidden, CodeForbidden, "app_not_permitted: an agent or session may not assert an app")
		return "", false
	}
	if !ok || !identity.IsOperatorCredential(p) {
		if ok {
			s.auditAppDropped(r, p)
		}
		return "", true
	}
	if !authissuer.ValidServiceApp(app) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "app must be one lowercase app id (letters, digits and '-', at most 32 characters)")
		return "", false
	}
	return app, true
}

func (s *Server) auditAppDropped(r *http.Request, p identity.Principal) {
	if s.IdentityAudit == nil {
		return
	}
	auth := "claimed"
	if p.Verified {
		auth = "verified"
	}
	family, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	_ = s.IdentityAudit(r.Context(), identity.Observation{
		At: time.Now().UTC(), PrincipalID: p.ID, SessionID: p.SessionID,
		Mode: s.IdentityMode, Authentication: auth,
		Method: r.Method, Route: "/" + family, Event: eventAppDropped,
	})
}
