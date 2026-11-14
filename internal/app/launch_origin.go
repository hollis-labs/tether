package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
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
	// Grant is a recorded authority (a principal's scopes) that bounds the launch
	// when there is no live origin session or verified caller, such as a team
	// session recovered or reconciled long after the verb that formed it.
	Grant *LaunchGrant
}

// LaunchGrant is the authority a launch was first made under, recorded so a
// later launch of the same thing can be bounded by it.
type LaunchGrant struct {
	Principal string
	Scopes    []string
}

// ErrLaunchRefused marks a launch the daemon refused because the authority it
// may hand out could not be established. It is never downgraded to an
// anonymous launch, in any identity mode.
var ErrLaunchRefused = errors.New("launch refused")

// launchScopeError is a launch whose authority ceiling left no usable scope: the
// launching credential holds none of the scopes the agent profile needs. It is a
// launch refusal with its own code (the API answers 409 launch_scope_unavailable),
// not an internal error, and it names only the profile's scope names.
type launchScopeError struct{ wanted []string }

func (e launchScopeError) Error() string {
	return "launch refused: the launching credential holds none of the scopes this agent needs (" + strings.Join(e.wanted, ", ") + "); launch from a credential that holds them"
}
func (launchScopeError) ErrorCode() string { return "launch_scope_unavailable" }
func (launchScopeError) Unwrap() error     { return ErrLaunchRefused }

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
	// AttributionCatalog: a chat launch of a catalog seat; the seat's catalog
	// definition bounds it, not the device that asked (CW-20261114-0063).
	AttributionCatalog Attribution = "catalog"
)

// chatLaunchKey marks a context as a Tachyon chat launch (deliverChat). Like the
// launch origin it is unexported: only LaunchChatSession sets it.
type chatLaunchKey struct{}

func withChatLaunch(ctx context.Context) context.Context {
	return context.WithValue(ctx, chatLaunchKey{}, true)
}

func isChatLaunch(ctx context.Context) bool {
	v, _ := ctx.Value(chatLaunchKey{}).(bool)
	return v
}

// LaunchChatSession launches a session created by the operator chat (POST
// /chat/{launch}/send) with the caller's context. A chat launch of a catalog
// seat is bounded by the seat's catalog definition, the same rule as a relaunch
// (CW-20261114-0038): the seat gets its catalog scopes, never more, whichever
// device asked (CW-20261114-0063).
func (s *Service) LaunchChatSession(ctx context.Context, sessionID string) (*Launched, error) {
	return s.LaunchSessionWithContext(withChatLaunch(ctx), sessionID)
}

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
	return s.launchCeilingFor(ctx, granted, narrowToLauncher)
}

// launchCeilingFor is launchCeiling with the launcher comparison chosen by the
// path: narrowToLauncher (hub-local) or narrowToLauncherWorker.
func (s *Service) launchCeilingFor(ctx context.Context, granted []string, narrow func(context.Context, []string) []string) ([]string, Attribution, error) {
	scopes := narrow(ctx, granted)
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
				scopes = retainHeld(scopes, held)
			}
		}
	}
	if internal && origin.Grant != nil {
		bounded = true
		if !slices.Contains(origin.Grant.Scopes, "*") {
			scopes = retainHeld(scopes, origin.Grant.Scopes)
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

// Audit event names for a launch that nothing bounded.
const (
	AuditAnonymousLaunch        = "anonymous_launch"
	AuditInternalUnattributedLn = "internal_unattributed"
)

// auditLaunchAttribution writes one identity_audit row for a session credential
// born without a bounding authority, so each one is counted and findable
// (anonymous_launch, or internal_unattributed). Launches a principal or an
// origin session bounded write nothing. A failure to audit is logged, not fatal:
// the credential itself carries the attribution that the enforce-flip guard reads.
func (s *Service) auditLaunchAttribution(ctx context.Context, sessionID, mode string, a Attribution) {
	var event string
	switch a {
	case AttributionAnonymous:
		event = AuditAnonymousLaunch
	case AttributionInternalUnattributed:
		event = AuditInternalUnattributedLn
	default:
		return
	}
	err := identity.NewStore(s.Store.DB()).RecordObservation(ctx, identity.Observation{
		At: time.Now().UTC(), PrincipalID: "msg://session/local/" + sessionID, SessionID: sessionID,
		Mode: identity.Mode(mode), Authentication: "none", Method: "LAUNCH", Route: "launch", Event: event,
	})
	if err != nil {
		log.Printf("identity: %s audit for session %s could not be persisted", event, sessionID)
	}
}

// Scope bases recorded for a worker session's grant (CW-20261114-0038).
const (
	// ScopeBasisLauncher: the launcher's token (and any origin) bounded it.
	ScopeBasisLauncher = "launcher"
	// ScopeBasisParentGrant: a resume of the same seat (same logical agent and
	// launch id) restored what its previous session held, capped by the catalog.
	ScopeBasisParentGrant = "parent_grant"
	// ScopeBasisCatalog: a chat launch of a catalog seat got the seat's catalog
	// scopes (CW-20261114-0063).
	ScopeBasisCatalog = "catalog"
)

// KindSessionScopesNarrowed is published when a launch drops scopes the
// session's catalog definition asked for.
const KindSessionScopesNarrowed = "session.scopes_narrowed"

// launchCeilingForSeat is the worker path's ceiling. A resume of the same seat
// (an internal resume origin, or a continuity "relaunch" create whose ended
// parent session, has this session's logical agent and launch id) keeps the scopes the seat's previous session was granted,
// capped by its catalog definition, whoever triggers the resume: a seat
// relaunched from Tachyon chat or a wake must not silently lose terminal/admin
// because the device that relaunched it holds less. Nobody gains scopes the
// seat never held: the parent's recorded grant is the ceiling, and that grant
// was itself bounded by whoever first launched the seat. Every other launch is
// launchCeilingFor (bounded by the launcher). Either way a dropped scope
// publishes session.scopes_narrowed.
func (s *Service) launchCeilingForSeat(ctx context.Context, row *store.SessionRow, plan *launch.Plan, granted []string, narrow func(context.Context, []string) []string) ([]string, Attribution, string, error) {
	scopes, attribution, basis, err := s.seatCeiling(ctx, row, plan, granted, narrow)
	if err == nil && row != nil {
		s.publishScopesNarrowed(ctx, row.ID, granted, scopes, basis)
	}
	return scopes, attribution, basis, err
}

func (s *Service) seatCeiling(ctx context.Context, row *store.SessionRow, plan *launch.Plan, granted []string, narrow func(context.Context, []string) []string) ([]string, Attribution, string, error) {
	if p, ok := identity.FromContext(ctx); ok && operatorChatCaller(p) && isChatLaunch(ctx) && s.catalogSeat(plan) {
		// A chat launch of a catalog seat by the operator: granted is the seat's
		// catalog mcp_scopes (or the default set), so nothing is added, and the
		// device that asked does not narrow it. It is checked before the parent
		// grant so the seat gets its catalog scopes whether or not an earlier
		// session of it was launched narrow. Any other caller, including one with
		// no principal (identity observe/off, which stays anonymous_external for
		// the enforce-flip guard), falls through to the launcher bound.
		return slices.Clone(granted), AttributionCatalog, ScopeBasisCatalog, nil
	}
	if parentID := s.seatParent(ctx, row, plan); parentID != "" {
		held, ok, err := identity.NewStore(s.Store.DB()).SessionGrantedScopes(ctx, parentID)
		if err != nil {
			return nil, "", "", fmt.Errorf("%w: the grant of origin session %q cannot be read: %w", ErrLaunchRefused, parentID, err)
		}
		if ok {
			scopes := slices.Clone(granted)
			if !slices.Contains(held, "*") {
				scopes = retainHeld(scopes, held)
			}
			return scopes, AttributionOrigin, ScopeBasisParentGrant, nil
		}
	}
	scopes, attribution, err := s.launchCeilingFor(ctx, granted, narrow)
	return scopes, attribution, ScopeBasisLauncher, err
}

// operatorChatCaller reports whether a chat launch's caller is the operator:
// the operator credential, or a device the operator paired (Tachyon) rather than
// a token minted from one. Only these get the catalog basis; any other holder of
// operate keeps the launcher bound, so launching a seat cannot widen what the
// caller holds (CW-20261114-0063 review). Same test as the #554 device read
// exception, without its read-scope requirement.
func operatorChatCaller(p identity.Principal) bool {
	if !p.Verified {
		return false
	}
	if p.ID == identity.OperatorID {
		return true
	}
	return p.Kind == "device" && p.TokenID == "" && p.CreatedBy == identity.OperatorID
}

// catalogSeat reports whether plan launches an agent defined in the live catalog.
func (s *Service) catalogSeat(plan *launch.Plan) bool {
	if plan == nil {
		return false
	}
	cat := s.cat()
	if cat == nil {
		return false
	}
	_, ok := cat.Agents[plan.CatalogAgent()]
	return ok
}

// seatParent returns the earlier session of the same seat whose grant caps
// this launch, or "" when the launch is not a relaunch of the same seat. Two
// launches qualify, both requiring the parent to have this row's logical agent
// and launch id:
//   - an internal resume (resume.go), whose origin names the source session;
//   - a create with continuity "relaunch" (CW-20261114-0038 follow-up) that
//     left the seat as the catalog defines it (plan.SeatRelaunch: no caller
//     boot prompt, agent, boot profile, override or injection) and, when the
//     caller named a worker, runs on the parent's worker. Its row records the
//     newest earlier session of the agent as its parent. That
//     parent is a newest-row heuristic, so it must also have ended or have a
//     stop requested (a worker session turns terminal only when the worker
//     reports the exit, after Tachyon's stop has returned): a relaunch never
//     copies the grant of a seat that is still running.
func (s *Service) seatParent(ctx context.Context, row *store.SessionRow, plan *launch.Plan) string {
	if row == nil || row.LogicalAgentID == "" || s.Store == nil {
		return ""
	}
	sameSeat := func(id string) *store.SessionRow {
		parent, err := s.Store.GetSession(id)
		if err != nil || parent == nil || parent.LogicalAgentID != row.LogicalAgentID || parent.LaunchID != row.LaunchID {
			return nil
		}
		return parent
	}
	if origin, internal := launchOriginFrom(ctx); internal && origin.Kind == OriginResume && origin.OriginSessionID != "" {
		if sameSeat(origin.OriginSessionID) != nil {
			return origin.OriginSessionID
		}
		return ""
	}
	if plan == nil || !plan.SeatRelaunch || row.Intent != "resume" || !row.ParentSessionID.Valid || row.ParentSessionID.String == row.ID {
		return ""
	}
	parent := sameSeat(row.ParentSessionID.String)
	if parent == nil || (!session.State(parent.State).Terminal() && !s.stops.requested(parent.ID)) {
		return ""
	}
	if plan.SeatRelaunchWorker && row.WorkerID != parent.WorkerID {
		return ""
	}
	return parent.ID
}

// publishScopesNarrowed reports the scopes a launch asked for but did not get.
func (s *Service) publishScopesNarrowed(ctx context.Context, sessionID string, requested, granted []string, basis string) {
	var dropped []string
	for _, sc := range requested {
		if !slices.Contains(granted, sc) {
			dropped = append(dropped, sc)
		}
	}
	if len(dropped) == 0 || s.Bus == nil {
		return
	}
	launcher := ""
	if p, ok := identity.FromContext(ctx); ok {
		launcher = p.ID
	}
	raw, _ := json.Marshal(map[string]any{"requested": requested, "granted": granted, "dropped": dropped, "basis": basis, "launcher": launcher})
	e := events.Event{Scope: events.ScopeSession, SessionID: sessionID, Kind: KindSessionScopesNarrowed, At: time.Now().UTC(), PayloadJSON: string(raw)}
	e.Envelope.Class.Category = "state"
	e.Envelope.Class.Severity = "warning"
	if err := s.Bus.Publish(context.WithoutCancel(ctx), e); err != nil {
		log.Printf("launch: publish %s for session %s: %v", KindSessionScopesNarrowed, sessionID, err)
	}
	log.Printf("launch: session %s scopes narrowed (basis %s): dropped %v", sessionID, basis, dropped)
}
