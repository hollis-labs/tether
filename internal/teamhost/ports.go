// Package teamhost implements inert team orchestration adapters. New constructs
// no daemon bindings: the owner supplies ports and schedules recovery explicitly.
package teamhost

import (
	"context"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

// Sessions retains keyed receipts even after an acknowledgement is lost. Stop
// atomically tombstones the intent and cleans only that intent's session, even
// before Launch acknowledges it. Late/future Launch calls cannot recreate it.
// Neither method may substitute another session of the same enrolled actor.
type Sessions interface {
	Launch(context.Context, SessionRequest) (string, error)
	Stop(context.Context, string) error
}
type SessionRequest struct {
	IntentKey     string
	Actor         mesh.URN
	ParentSession string
	Provision     teams.ProvisionRequest
}

// Enroller verifies a supplied definition pin during Ensure; stable identities
// without a supplied pin resolve their own enrolled definition. Stable resolutions
// require an already enrolled requested identity; fresh mints an
// ephemeral identity keyed to the intent (Actor must be empty). All methods are durable/idempotent.
// AcquireBinding is exclusive by actor across all consumers. ReleaseBinding
// permanently fences acquisition for that intent and releases only its lease.
// Release/Retire permanently fence Ensure; Retire removes only the ephemeral
// enrollment owned by the key. Late calls after cleanup must remain fenced.
type Enroller interface {
	Ensure(context.Context, EnrollmentRequest) (Enrollment, error)
	AcquireBinding(context.Context, string, mesh.URN) error
	ReleaseBinding(context.Context, string) error
	Release(context.Context, string) error
	Retire(context.Context, string) error
}
type EnrollmentRequest struct {
	IntentKey string
	Actor     mesh.URN
	Provision teams.ProvisionRequest
}
type Enrollment struct {
	Actor        mesh.URN
	AgentID      string
	Kind         mesh.ActorKind
	Ephemeral    bool
	SpawnCapable bool
}

// Messenger durably deduplicates by Delivery.IdempotencyKey and binds the full
// content. Success means accepted by the retained session, including durable
// queueing for DeliveryAtIdle. Unavailable sessions/unsupported policies return
// errors; the adapter must never retarget to a newer session of the same actor.
// ErrSessionGone means the retained session ended and becomes a dead letter.
// Temporary transport failures use ordinary retryable errors; ErrPermanent marks
// another permanent refusal. No delivery policy may substitute immediate send.
type Messenger interface {
	Deliver(context.Context, teams.Delivery) error
}

// Channels only derives a name; it must not create a channel or publish content.
type Channels interface{ Name(string) (string, error) }

type Ports struct {
	Sessions  Sessions
	Enroller  Enroller
	Messenger Messenger
	Channels  Channels
}
type TrustTier string

const (
	TrustDenied   TrustTier = "denied"
	TrustApproval TrustTier = "approval"
	TrustTrusted  TrustTier = "trusted"
)

type Options struct {
	// TrustTiers uses exact definition pins for both parent and target. Omission
	// denies; any approval tier needs approval; only two trusted tiers allow.
	TrustTiers map[mesh.DefinitionRef]TrustTier
	// ActorTrust admits explicitly configured principals without a provision intent.
	ActorTrust map[mesh.URN]TrustTier
	Now        func() time.Time
}
