// Package teamsvc implements authenticated team operations independently of
// transport and daemon wiring. Hosts supply durable storage and runtime ports.
package teamsvc

import (
	"context"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
)

const LocalOperator mesh.URN = "msg://user/local/operator"

type Principal struct {
	ID   mesh.URN
	Kind mesh.ActorKind
	// Verified is identity.Principal.Verified, copied by the host adapter.
	// LocalOperator is a different, stronger proof (Unix-socket peer) and is
	// why the operator may act without a bearer on that path.
	Verified      bool
	LocalOperator bool
}

// Principals reads trusted authentication context, never request actor fields.
// LocalOperator may be true only after proving a local-socket connection by
// operator credential or peer identity. An asserted header, query parameter or
// request field is never proof. The service does not compute verification.
type Principals interface {
	ResolvePrincipal(context.Context) (Principal, error)
}

// Runs returns immutable library metadata. Channel transport naming belongs
// to the host; it must not rewrite the library's run channel.
type Runs interface {
	GetRun(context.Context, string) (teams.TeamRun, error)
	MarkDissolved(context.Context, string) error
}

// Adopter is the host seam that adopts an already-running actor and session as
// a member. The host derives every fact from the session's own records; nothing
// in the command is trusted. Adopt is idempotent by Key. Nil makes Adopt
// unavailable.
type Adopter interface {
	Adopt(context.Context, AdoptCommand) (teams.Member, error)
}

// AdoptCommand is what the service has already authorized for one adoption.
type AdoptCommand struct {
	Key, MemberID, RunID string
	Slot                 teams.Slot
	Actor                mesh.URN
	SessionID            string
	BindingGeneration    uint64
	// Principal is the forming principal; the binding must be owned by it.
	Principal mesh.URN
	Limits    mesh.Limits
}

type Deps struct {
	Ceilings    Ceilings
	Principals  Principals
	Runs        Runs
	Calls       Calls
	Definitions teams.DefinitionStore
	Roster      teams.RosterStore
	Ledger      teams.LaunchLedger
	Signals     teams.SignalStore
	Provisioner teams.MemberProvisioner
	Adopter     Adopter
	Workflows   teams.WorkflowLauncher
	Triggers    teams.TriggerEvaluator
	Sender      teams.MessageSender
	Routing     teams.RoutingInstaller
	Trust       teams.TrustResolver
	Approvals   teams.ApprovalEmitter
	Clock       teams.Clock
	IDs         teams.IDs
	Defaults    mesh.Limits
}
