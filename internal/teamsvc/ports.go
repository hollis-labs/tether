// Package teamsvc implements authenticated team operations independently of
// transport and daemon wiring. Hosts supply durable storage and runtime ports.
package teamsvc

import (
	"context"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

const LocalOperator mesh.URN = "msg://user/local/operator"

type Principal struct {
	ID            mesh.URN
	Kind          mesh.ActorKind
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
