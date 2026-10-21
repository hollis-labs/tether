package app

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// Turn input provenance (CW-20261021-0010). Every input handed to a session is
// recorded before the text reaches the runtime, with a sender class derived from
// AUTHENTICATED facts only: a verified principal, or a sender the hub stamped on a
// message. Prose, timestamps and the copied owner-inbox destination are never
// evidence. Any doubt resolves to "unknown", never to "operator". This is a DEV
// policy, to be revisited at release posture.

type turnInputKey struct{}

// turnInputNote is what a caller knows about the input it is about to submit.
// An empty Class means "derive it from the principal in the context".
type turnInputNote struct {
	Source    string
	SenderURN string
	Class     string
	Basis     string
	RefID     string
}

func withTurnInput(ctx context.Context, n turnInputNote) context.Context {
	return context.WithValue(ctx, turnInputKey{}, n)
}

func turnInputFrom(ctx context.Context) (turnInputNote, bool) {
	n, ok := ctx.Value(turnInputKey{}).(turnInputNote)
	return n, ok
}

// systemInput marks an injection Tether makes on its own (a codex prime, a boot
// continuation): the class is system, the basis is the injection itself.
func systemInput(ref string) turnInputNote {
	return turnInputNote{Source: store.InputSystem, Class: store.ClassSystem, Basis: store.BasisSystem, RefID: ref}
}

// classifyStampedSender classifies the author of a stored message (a mail wake, a
// routing reply) from what the daemon verified, not from what the message claims.
//
// The From address is a claim: a principal holding "*" or message.send_as (the
// shared operator credential, for one) may author ANY From, so "verified" proves
// only that the credential was allowed to claim it, never who wrote the mail. The
// evidence is therefore the verified principal the daemon recorded
// (from_principal), and:
//
//   - a session or agent principal: agent (it can only author its own addresses
//     unless it holds send_as, and either way the author is that agent);
//   - a service principal: system;
//   - the shared operator credential: unknown. A human at the CLI and an agent on the
//     shared operator-authorized MCP connection are indistinguishable behind it, so
//     the credential's scope is never taken as evidence of a human;
//   - a device principal: unknown (its scopes are not in the message stamp);
//   - no stamp, an "unverified" stamp, or no recorded principal: unknown.
//
// Anything not proven is unknown, never operator.
func classifyStampedSender(from messaging.Address, meta map[string]string) (urn, class, basis string) {
	switch meta[api.MetaFromVerification] {
	case "stamped", "verified":
	default: // absent or "unverified"
		return "", store.ClassUnknown, store.BasisNone
	}
	principal, err := messaging.ParseURN(meta[api.MetaFromPrincipal])
	if err != nil {
		return "", store.ClassUnknown, store.BasisNone
	}
	urn = principal.URN()
	switch {
	case urn == identity.OperatorID:
		return urn, store.ClassUnknown, store.BasisSharedOperator
	case principal.Kind == messaging.KindAgent || principal.Kind == messaging.KindSession:
		return urn, store.ClassAgent, store.BasisStampedSender
	case principal.Kind == messaging.KindService:
		return urn, store.ClassSystem, store.BasisStampedSender
	}
	return urn, store.ClassUnknown, store.BasisNone
}

// beginTurnInput records the input about to be submitted to sessionID and returns
// the function that settles it: accepted when the runtime took the text, failed
// otherwise. A recording failure is logged and never blocks the turn.
func (s *Service) beginTurnInput(ctx context.Context, sessionID string) func(error) {
	noop := func(error) {}
	if s.Store == nil || sessionID == "" || strings.HasPrefix(sessionID, "outpost:") {
		return noop
	}
	note, annotated := turnInputFrom(ctx)
	if !annotated || note.Source == "" {
		note.Source = identity.InputSourceFrom(ctx)
		if note.Source == "" {
			note.Source = store.InputSessionTurn
		}
	}
	if note.Class == "" {
		p, ok := identity.FromContext(ctx)
		note.SenderURN, note.Class, note.Basis = identity.ClassifyInputSender(p, ok)
	}
	rec, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	in, err := s.Store.RecordTurnInput(rec, store.TurnInput{SessionID: sessionID, Source: note.Source, SenderURN: note.SenderURN, SenderClass: note.Class, Basis: note.Basis, RefID: note.RefID})
	cancel()
	if err != nil {
		log.Printf("turn input: record for session %s failed: %v", sessionID, err)
		return noop
	}
	return func(submitErr error) {
		var ran *turnRanError
		accepted := submitErr == nil || errors.As(submitErr, &ran)
		settle, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		if err := s.Store.SettleTurnInput(settle, in.ID, accepted); err != nil {
			log.Printf("turn input: settle %s failed: %v", in.ID, err)
		}
	}
}

// audienceFor binds the oldest recorded input of the session to turnID and returns
// what the routed output may say about it. It runs on the output path, so it is
// bounded by the same persistence deadline as the rest of publication and never
// holds the reader: if the binding cannot complete in time, the output says
// "unknown" with the reason, and is never labeled from a later or different input.
func (s *Service) audienceFor(sessionID, turnID string) *store.TurnAudience {
	if s.Store == nil {
		return nil
	}
	ctx, cancel := s.outputPersistenceContext()
	defer cancel()
	a, err := s.Store.BindTurnInput(ctx, sessionID, turnID)
	if err != nil {
		log.Printf("turn input: bind for session %s turn %s failed: %v", sessionID, turnID, err)
		u := store.UnknownAudience("binding_unavailable")
		return &u
	}
	return &a
}

// launchPromptRef marks the input that is a session's creation prompt; it is
// recorded when the session is created and settled when its launch finishes.
const launchPromptRef = "launch_prompt"

func (s *Service) recordLaunchPromptInput(sessionID string, o *identity.InputOrigin) {
	if s.Store == nil || sessionID == "" || o == nil {
		return
	}
	class, basis := o.Class, o.Basis
	if class == "" {
		class, basis = store.ClassUnknown, store.BasisNone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.Store.RecordTurnInput(ctx, store.TurnInput{SessionID: sessionID, Source: o.Source, SenderURN: o.SenderURN, SenderClass: class, Basis: basis, RefID: launchPromptRef}); err != nil {
		log.Printf("turn input: record launch prompt for session %s failed: %v", sessionID, err)
	}
}

func (s *Service) settleLaunchPromptInput(sessionID string, launched bool) {
	if s.Store == nil || sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Store.SettleLaunchPromptInput(ctx, sessionID, launched); err != nil {
		log.Printf("turn input: settle launch prompt for session %s failed: %v", sessionID, err)
	}
}
