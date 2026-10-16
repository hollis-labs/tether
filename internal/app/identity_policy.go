package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gomsg "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

// Handler-level audit events written by IdentityPolicy. "_unauthorized" rows
// are observe-mode findings (the request proceeded); "_refused" rows are
// enforce-mode refusals.
const (
	eventReplyUnauthorized    = "reply_unauthorized"
	eventReplyRefused         = "reply_refused"
	eventDeliveryUnauthorized = "delivery_unauthorized"
	eventDeliveryRefused      = "delivery_refused"
	eventChannelUnauthorized  = "channel_unauthorized"
	eventChannelRefused       = "channel_refused"
)

// ErrNotParticipant is the cause of a refused reply or delivery read.
var ErrNotParticipant = errors.New("caller is not a participant of the message")

// IdentityPolicy is the caller-identity policy for reply routing, delivery
// reads and channel publication (CW-20261010-0266). Mode is the effective mode
// of the local_api point: Observe records what Enforce would refuse and lets
// the request through; Enforce refuses it. Off installs nothing (see Install).
type IdentityPolicy struct {
	Mode identity.Mode
	// Audit persists a finding. A nil Audit drops findings.
	Audit func(context.Context, identity.Observation) error
	// InternalPublishers are the daemon-internal service addresses allowed to
	// publish to a channel on behalf of a session without holding any scope.
	// The rule is explicit: an empty list authorizes no internal publisher.
	InternalPublishers []string
}

// NewIdentityPolicy returns the policy with the turn router explicitly
// authorized as an internal publisher.
func NewIdentityPolicy(mode identity.Mode, audit func(context.Context, identity.Observation) error, internalPublishers ...string) *IdentityPolicy {
	return &IdentityPolicy{Mode: mode, Audit: audit, InternalPublishers: internalPublishers}
}

func (p *IdentityPolicy) enforcing() bool { return p.Mode == identity.Enforce }

// finding records a would-deny (observe) or a refusal (enforce) and returns the
// error to hand back: nil in observe mode, cause in enforce mode.
func (p *IdentityPolicy) finding(ctx context.Context, caller identity.Principal, observed, refused string, cause error) error {
	event := observed
	if p.enforcing() {
		event = refused
	}
	// Anonymous, claim-only traffic is not audited in observe mode: it must not
	// allocate audit rows (middleware.go). An enforce refusal is audited when
	// there is a principal to attribute.
	if p.Audit != nil && caller.ID != "" && (caller.Verified || p.enforcing()) {
		auth := "claimed"
		if caller.Verified {
			auth = "verified"
		}
		_ = p.Audit(ctx, identity.Observation{At: time.Now().UTC(), PrincipalID: caller.ID, SessionID: caller.SessionID,
			Mode: p.Mode, Authentication: auth, Route: "/messages", Event: event})
	}
	if p.enforcing() {
		return cause
	}
	return nil
}

// participants are the addresses entitled to act on a routed message: its
// sender, its recipient when that is a mailbox (a channel has no member list,
// so a channel recipient confers nothing), and the principal that sent it.
func routedParticipants(parent gomsg.Envelope) []string {
	var out []string
	if !parent.From.IsZero() {
		out = append(out, parent.From.URN())
	}
	if _, isChannel := channels.AddressName(parent.To); !isChannel && !parent.To.IsZero() {
		out = append(out, parent.To.URN())
	}
	if fp := parent.Metadata[apiMetaFromPrincipal]; fp != "" {
		out = append(out, fp)
	}
	return out
}

// apiMetaFromPrincipal mirrors api.MetaFromPrincipal (the stamped sending principal).
const apiMetaFromPrincipal = "from_principal"

func mayActAsAny(caller identity.Principal, addrs []string) bool {
	if !caller.Verified {
		return false
	}
	for _, a := range addrs {
		if caller.MayActAs(a) {
			return true
		}
	}
	// message.send_as / "*" holders pass MayActAs for any non-empty address;
	// with no participant at all, only they may act.
	return len(addrs) == 0 && caller.MayActAs("")
}

// Reply is the ReplyAuthorization: a verified caller may reply to a routed
// message only as its sender, its mailbox recipient or the sending principal,
// or holding message.send_as.
func (p *IdentityPolicy) Reply() ReplyAuthorization {
	return func(ctx context.Context, caller identity.Principal, parent gomsg.Envelope, _ string) error {
		if mayActAsAny(caller, routedParticipants(parent)) {
			return nil
		}
		return p.finding(ctx, caller, eventReplyUnauthorized, eventReplyRefused, ErrNotParticipant)
	}
}

// Delivery is the DeliveryAuthorization: the reply's author, the parent's
// participants, and the sessions the reply was addressed or delivered to.
func (p *IdentityPolicy) Delivery() DeliveryAuthorization {
	return func(ctx context.Context, caller identity.Principal, reply store.RoutingReply, parent gomsg.Envelope) error {
		addrs := routedParticipants(parent)
		if reply.Actor != "" {
			addrs = append(addrs, reply.Actor)
		}
		for _, sid := range []string{reply.OriginalSessionID, reply.TargetSessionID, reply.DeliveredToSessionID} {
			if sid != "" {
				addrs = append(addrs, "msg://session/local/"+sid)
			}
		}
		if mayActAsAny(caller, addrs) {
			return nil
		}
		return p.finding(ctx, caller, eventDeliveryUnauthorized, eventDeliveryRefused, ErrNotParticipant)
	}
}

// Channel is the channels.Authorization installed on the store's publication
// seam and on the channel service. Publication is allowed for:
//   - a listed internal publisher (the turn router) publishing as a session
//     address, with no scope required (explicit rule, process-local marker);
//   - a verified principal authoring a from within its own address set.
//
// Reads (list, history, subscribe) need a verified principal; read isolation
// between verified principals is CW-20261001-0263.
func (p *IdentityPolicy) Channel() channels.Authorization {
	return func(ctx context.Context, operation, name string, caller identity.Principal, from gomsg.Address) error {
		if operation == "publish" {
			if svc, ok := identity.InternalService(ctx); ok && p.internalPublisher(svc) && from.Kind == gomsg.KindSession && caller.ID == from.URN() {
				return nil
			}
			if caller.Verified && !from.IsZero() && caller.MayActAs(from.URN()) {
				return nil
			}
		} else if caller.Verified {
			return nil
		}
		return p.finding(ctx, caller, eventChannelUnauthorized, eventChannelRefused,
			fmt.Errorf("%w: %s on channel %q", channels.ErrForbidden, operation, name))
	}
}

func (p *IdentityPolicy) internalPublisher(urn string) bool {
	for _, allowed := range p.InternalPublishers {
		if strings.EqualFold(allowed, urn) {
			return true
		}
	}
	return false
}

// InstallIdentityPolicy installs policy on the reply, delivery and channel
// seams. An Off policy installs nothing. Call once during composition, before
// the listener accepts requests.
func (s *Service) InstallIdentityPolicy(policy *IdentityPolicy) {
	if policy == nil || policy.Mode == identity.Off || policy.Mode == "" {
		return
	}
	s.ReplyAuthorization = policy.Reply()
	s.DeliveryAuthorization = policy.Delivery()
	if s.Store != nil {
		s.Store.SetChannelAuthorization(policy.Channel())
	}
}
