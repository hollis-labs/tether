package teamruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// AdoptionDirectory is the registry slice the adopter reads. *LegacyEnroller
// provides it: registry access stays inside the legacy adapter files.
type AdoptionDirectory interface {
	AdoptionActor(ctx context.Context, actor mesh.URN) (AdoptionActor, error)
	AdoptionBinding(ctx context.Context, actor mesh.URN, now time.Time) (AdoptionBinding, error)
}

// AdoptionSessions reads a session's own launch record.
type AdoptionSessions interface {
	GetSession(id string) (*store.SessionRow, error)
}

// SessionOwners names the principal that launched a session, from the
// session's own live credential. Empty means unverified.
type SessionOwners interface {
	SessionOwner(ctx context.Context, sessionID string) (string, error)
}

// AdoptionNotices sends the one keyed notice an adopted member receives when it
// is released.
type AdoptionNotices interface {
	SendKeyedMessage(ctx context.Context, key string, env messaging.Envelope) (messaging.Envelope, error)
}

// RegistryAdopter proves an adoption from the daemon's own records: the actor's
// registration, its current registry binding (session and generation), the
// session row (launch id, liveness) and the session's live credential (who
// launched it). Nothing is taken from the request. Release revokes nothing in
// the registry or the session: an adopted member holds no team-made resource;
// leaving the roster is the whole revocation, and the member is told once.
type RegistryAdopter struct {
	Directory AdoptionDirectory
	Sessions  AdoptionSessions
	Owners    SessionOwners
	Notices   AdoptionNotices
	// Launches maps an allowlisted launch id to the exact definition pin the
	// teams policy binds to it. A launch that is absent here has no pin.
	Launches teamhost.AdoptedLaunches
	// Definitions, when set, must load every pin returned (content verified).
	Definitions Definitions
	// From is the notice sender: the local operator.
	From mesh.URN
	Now  func() time.Time
}

var _ teamhost.Adopter = (*RegistryAdopter)(nil)

func (a *RegistryAdopter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Verify reads the records and mutates nothing. A missing or unverifiable
// fact yields Resolved=false or a typed refusal, never a guess.
func (a *RegistryAdopter) Verify(ctx context.Context, c teamhost.AdoptionCheck) (teamhost.Adoption, error) {
	if a.Directory == nil || a.Sessions == nil || a.Owners == nil {
		return teamhost.Adoption{}, teams.ErrUnavailable
	}
	profile, err := a.Directory.AdoptionActor(ctx, c.Actor)
	if err != nil {
		return teamhost.Adoption{}, err
	}
	if !profile.ActiveAgent || profile.URN != string(c.Actor) {
		return teamhost.Adoption{}, fmt.Errorf("%w: %s is not an active enrolled agent", teams.ErrProvisionFailed, c.Actor)
	}
	out := teamhost.Adoption{Actor: c.Actor, AgentID: profile.URN, Kind: mesh.ActorAgent}
	binding, err := a.Directory.AdoptionBinding(ctx, c.Actor, a.now())
	if err != nil {
		return teamhost.Adoption{}, err
	}
	if !binding.Live || binding.SessionID == "" {
		return out, nil // not bound, revoked or expired: Resolved stays false
	}
	row, err := a.Sessions.GetSession(binding.SessionID)
	if errors.Is(err, store.ErrSessionNotFound) {
		return out, nil
	}
	if err != nil {
		return teamhost.Adoption{}, err
	}
	if session.State(row.State).Terminal() {
		return out, nil
	}
	owner, err := a.Owners.SessionOwner(ctx, binding.SessionID)
	if err != nil {
		return teamhost.Adoption{}, err
	}
	// live guarantees Generation > 0, so the conversion cannot wrap.
	out.SessionID, out.Generation, out.LaunchID, out.Owner = binding.SessionID, uint64(binding.Generation), row.LaunchID, mesh.URN(owner) //nolint:gosec // Generation > 0 checked above
	out.Pin = a.Launches[row.LaunchID]
	// Resolved means the binding is live, the session runs, and its own live
	// credential says who launched it. Without that credential (observe mode)
	// nobody is vouching for the session, so it is not adoptable.
	out.Resolved = owner != ""
	if out.Resolved && out.Pin.ID != "" && a.Definitions != nil {
		if _, err = a.Definitions.Load(ctx, out.Pin); err != nil {
			return teamhost.Adoption{}, fmt.Errorf("%w: launch %q pin does not load: %w", teams.ErrDenied, row.LaunchID, err)
		}
	}
	return out, nil
}

// Release tells the member once, keyed by the intent, that it has left the run.
// It never touches the session, its binding or its registration.
func (a *RegistryAdopter) Release(ctx context.Context, intentKey string, m teams.Member) error {
	if a.Notices == nil {
		return nil
	}
	from, err := messaging.ParseURN(string(a.From))
	if err != nil {
		return fmt.Errorf("adoption notice sender: %w", err)
	}
	to, err := messaging.ParseURN(string(m.Actor))
	if err != nil {
		return fmt.Errorf("adoption notice recipient: %w", err)
	}
	body, err := json.Marshal(struct {
		Body string `json:"body"`
	}{"You have been released from a team run. Your session, registration and work are unchanged; you may be adopted again."})
	if err != nil {
		return err
	}
	_, err = a.Notices.SendKeyedMessage(ctx, "team-adopt-release:"+intentKey, messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: body, ContentType: "application/json", Metadata: map[string]string{"team_adoption": "released", "team_member": m.ID}})
	return err
}

// HostAdoptions turns an authorized service command into a host adoption, the
// production wiring between teamsvc and teamhost.
type HostAdoptions struct{ Host *teamhost.Host }

var _ teamsvc.Adopter = HostAdoptions{}

func (h HostAdoptions) Adopt(ctx context.Context, c teamsvc.AdoptCommand) (teams.Member, error) {
	if h.Host == nil {
		return teams.Member{}, teams.ErrUnavailable
	}
	return h.Host.Adopt(ctx, teamhost.AdoptRequest{
		Provision:         teams.ProvisionRequest{IdempotencyKey: c.Key, RunID: c.RunID, MemberID: c.MemberID, Slot: c.Slot, Identity: c.Actor, Limits: c.Limits},
		SessionID:         c.SessionID,
		BindingGeneration: c.BindingGeneration,
		Principal:         c.Principal,
	})
}
