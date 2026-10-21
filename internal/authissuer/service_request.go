package authissuer

import (
	"slices"

	"github.com/hollis-labs/tether/internal/identity"
)

// A session asks for a service token for its own app (CW-20261020-0036). This is a
// narrow, separate path beside the operator's IssueService: Exchange is unchanged and
// still refuses agent and session parents, and RequestService never takes an operator
// principal. It reuses mintService, so the signing key, the record, the deny-list and
// LiveService behave exactly as for an operator-issued token.

// maxRequestedPerSession bounds the live tokens one session may hold for its app.
const maxRequestedPerSession = 5

// sessionRequestScopes is the dev allowlist of scopes a session may request for its own
// app: publish events as the app. Nothing else, no wildcard, and no scope on another
// audience (an app-own <app>.<verb> scope would be added here deliberately).
var sessionRequestScopes = []string{"tether.events"}

// ValidServiceApp reports whether app can be the app of a service principal: the id
// shape a service token accepts, and not a reserved issuer subject.
func ValidServiceApp(app string) bool {
	return audRe.MatchString(app) && !identity.ReservedIssuerSubject(ServicePrincipalID(app, DefaultServiceEnv))
}

// SessionCaller is the session asking for a token. The hub fills App and Ended from the
// session row it loads by the verified session id; nothing here comes from the request.
type SessionCaller struct {
	// PrincipalID is the verified principal of the caller (recorded as issuer and par).
	PrincipalID string
	SessionID   string
	// App is the session's stamped app ("" when it has none).
	App   string
	Ended bool
}

// SessionServiceRequest is what the session itself chooses: scopes and a label.
type SessionServiceRequest struct {
	Scopes []string
	Label  string
}

// RequestService mints a service token for the session's own app. It issues only
// msg://service/<session.App>/dev for audience tether, with scopes inside
// sessionRequestScopes (tether.events when none are named).
func (i *Issuer) RequestService(c SessionCaller, req SessionServiceRequest) (Token, ServiceRecord, error) {
	canon, err := checkSessionRequest(c, req)
	if err != nil {
		return Token{}, ServiceRecord{}, err
	}
	sreq := ServiceRequest{App: c.App, Audience: TetherAudience, Scopes: canon, Label: req.Label}
	return i.mintService(c.PrincipalID, sreq, "", false, c.PrincipalID, c.SessionID, func(r ServiceRecord) error {
		return i.State.addServiceForRequester(r, maxRequestedPerSession)
	})
}

// CheckSessionRequest applies RequestService's rules without minting.
func CheckSessionRequest(c SessionCaller, req SessionServiceRequest) error {
	_, err := checkSessionRequest(c, req)
	return err
}

func checkSessionRequest(c SessionCaller, req SessionServiceRequest) ([]string, error) {
	if c.SessionID == "" || c.PrincipalID == "" {
		return nil, forbidden("a verified session is required")
	}
	if c.Ended {
		return nil, forbidden("the session has ended")
	}
	if c.App == "" {
		return nil, forbidden("this session has no app; the operator stamps one when it is launched")
	}
	if !ValidServiceApp(c.App) {
		return nil, forbidden("this session's app cannot hold a service token")
	}
	if req.Label != "" && !labelRe.MatchString(req.Label) {
		return nil, invalid("label must be at most 64 plain characters")
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = []string{"tether.events"}
	}
	canon, err := identity.CanonicalScopes(scopes)
	if err != nil {
		return nil, invalid("unknown tether scope")
	}
	for _, sc := range canon {
		if !slices.Contains(sessionRequestScopes, sc) {
			return nil, forbidden("a session may request only %v", sessionRequestScopes)
		}
	}
	return canon, nil
}
