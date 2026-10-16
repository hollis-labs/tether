package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"

	"github.com/hollis-labs/tether/internal/identity"
)

// LaunchOriginKind names why the daemon itself started a session.
type LaunchOriginKind string

const (
	// OriginResume is a session continued from an earlier one.
	OriginResume LaunchOriginKind = "resume"
	// OriginTeam is a team member spawned by (or replacing) another session.
	OriginTeam LaunchOriginKind = "team"
)

// InternalLaunch says a launch was started by the daemon's own code, not by an
// external request, and which session it descends from. It bounds the new
// session's scopes to that session's credential, so an internal launch can
// never mint more authority than its origin held.
type InternalLaunch struct {
	Kind LaunchOriginKind
	// OriginSessionID is the session whose credential bounds the launch:
	// the resumed session, or the team member's parent. Empty means the launch
	// has no origin session (it keeps the profile ceiling and is reported as
	// internal_unattributed).
	OriginSessionID string
}

// ErrLaunchRefused marks a launch the daemon refused because the authority it
// may hand out could not be established. It is never downgraded to an
// anonymous launch, in any identity mode.
var ErrLaunchRefused = errors.New("launch refused")

// Attribution says what vouched for a launch's scopes.
type Attribution string

const (
	// AttributionPrincipal: a verified principal asked for the launch.
	AttributionPrincipal Attribution = "principal"
	// AttributionOrigin: no principal, but the daemon's own origin session bounds it.
	AttributionOrigin Attribution = "origin"
	// AttributionInternalUnattributed: the daemon launched it with no origin
	// that can bound it; it keeps the profile ceiling.
	AttributionInternalUnattributed Attribution = "internal_unattributed"
	// AttributionAnonymous: an external launch with no verified principal. Only
	// possible while identity is observe or off.
	AttributionAnonymous Attribution = "anonymous_external"
)

// launchOriginKey is unexported, and so is the setter: only code in this
// package can mark a context as an internal launch. Nothing that serves a
// request can, which is what separates a daemon-internal launch from an
// anonymous external one (see TestOnlyDaemonCompositionCanSetALaunchOrigin).
type launchOriginKey struct{}

func withLaunchOrigin(ctx context.Context, o InternalLaunch) context.Context {
	return context.WithValue(ctx, launchOriginKey{}, o)
}

func launchOriginFrom(ctx context.Context) (InternalLaunch, bool) {
	o, ok := ctx.Value(launchOriginKey{}).(InternalLaunch)
	return o, ok
}

// LaunchInternal starts a created session on behalf of the daemon's own code
// (the team host), bounded by its origin session. It must not be called from
// anything that serves a request; a boundary test enforces that for the api,
// mcpadapter, daemon and ACP packages.
func (s *Service) LaunchInternal(ctx context.Context, sessionID string, o InternalLaunch) (*Launched, error) {
	return s.LaunchSessionWithContext(withLaunchOrigin(ctx, o), sessionID)
}

// launchCeiling returns the scopes a new session may hold and what vouched for
// them. It is the one decision both credential paths (hub-local and worker)
// make, so they cannot drift:
//
//   - a verified launcher bounds it (narrowToLauncher);
//   - an internal origin session bounds it too: min(profile, the origin's
//     credential), where the origin's grant is read even after its credential
//     was revoked or expired, because a resume continues work its creator
//     started;
//   - with neither, the profile ceiling stands, and the result is either
//     internal_unattributed (the daemon launched it) or anonymous_external.
//
// It fails closed: if an internal launch names an origin session and that
// session's grant cannot be read (a locked database, a malformed record), the
// launch is refused. A transient error must never widen authority. The one
// deliberate exception is an origin session that never had a credential at all
// (it predates session credentials, or identity was off when it started): there
// is no grant to bound by, so the profile ceiling stands as it does for any
// launch with no origin, the case is logged, and it is classed
// internal_unattributed.
func (s *Service) launchCeiling(ctx context.Context, granted []string) ([]string, Attribution, error) {
	scopes := narrowToLauncher(ctx, granted)
	_, hasPrincipal := identity.FromContext(ctx)
	origin, internal := launchOriginFrom(ctx)
	bounded := false
	if internal && origin.OriginSessionID != "" && s.Store != nil {
		held, ok, err := identity.NewStore(s.Store.DB()).SessionGrantedScopes(ctx, origin.OriginSessionID)
		if err != nil {
			return nil, "", fmt.Errorf("%w: the grant of origin session %q cannot be read: %w", ErrLaunchRefused, origin.OriginSessionID, err)
		}
		if !ok {
			log.Printf("launch origin session %q has no recorded credential; the profile ceiling applies (internal_unattributed)", origin.OriginSessionID)
		} else {
			bounded = true
			if !slices.Contains(held, "*") {
				var narrowed []string
				for _, scope := range scopes {
					if slices.Contains(held, scope) {
						narrowed = append(narrowed, scope)
					}
				}
				scopes = narrowed
			}
		}
	}
	switch {
	case hasPrincipal:
		return scopes, AttributionPrincipal, nil
	case bounded:
		return scopes, AttributionOrigin, nil
	case internal:
		return scopes, AttributionInternalUnattributed, nil
	default:
		return scopes, AttributionAnonymous, nil
	}
}
