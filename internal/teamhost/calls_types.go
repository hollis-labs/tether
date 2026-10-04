package teamhost

import (
	"context"
	"encoding/json"

	"github.com/hollis-labs/substrate/mesh"
)

// Scope binds a caller key across runs. Reusing it for different content conflicts.
type Scope struct {
	Principal mesh.URN
	Verb      string
	Key       string
}
type Intent struct {
	Scope   Scope
	Digest  string
	Request json.RawMessage
}
type Record struct {
	Intent Intent
	Plan   json.RawMessage
	Result json.RawMessage
}

// Calls runs inside the ledger lease for ServiceKey(scope). Intent, plan and
// result bytes are immutable and detached. Plan/result writes are first-wins;
// changed bytes conflict. Records are retained until explicit host policy allows
// expiry. Unknown record keys return teams.ErrNotFound.
type Calls interface {
	GetOrCreate(context.Context, Intent) (Record, error)
	SetPlan(context.Context, Scope, json.RawMessage) error
	Complete(context.Context, Scope, json.RawMessage) error
}
