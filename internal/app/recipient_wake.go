package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func (s *Service) installRecipientWake() {
	s.Store.SetMessageDeliveryObserver(func(ctx context.Context, env messaging.Envelope) string {
		if wakeintent.IsLocalNotify(ctx) {
			return ""
		}
		// A send-time wake is a mail wake: tag its turn input like the sweep's
		// and notify's (it was recorded as a plain session turn).
		out := recipientWake(s.mailWakeInputContext(ctx, env.ID), s.Store, s.Registry, s.runtimeSeam(), env)
		raw, _ := json.Marshal(out)
		return string(raw)
	})
}

func recipientWake(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, env messaging.Envelope) (out api.WakeOutcome) {
	delegated := false
	defer func() {
		if !delegated && st != nil {
			if err := st.RecordMessageWake(context.WithoutCancel(ctx), env.ID, env.To.URN(), "wake.outcome", "", 0, 0, out.Attempted, out.Delivered, out.Reason); err != nil {
				log.Printf("app: record recipient wake outcome: %v", err)
			}
		}
	}()
	intent, err := wakeintent.Read(env)
	if err != nil {
		return api.WakeOutcome{Reason: "invalid-wake-intent", Detail: err.Error()}
	}
	if intent.NoWake {
		return api.WakeOutcome{Reason: "no-wake"}
	}
	// A duplicate send of an already admitted message reports that message's
	// stored wake outcome (CW-20261118-0024): triage and session resolution
	// would otherwise answer for a second wake that admission never allows.
	if prior, ok := storedRecipientWake(ctx, st, env.ID); ok {
		delegated = true
		return prior
	}
	if d := rt.triage.gate(ctx, env); !d.wake {
		return api.WakeOutcome{Reason: d.reason}
	}
	sessionID, err := resolveWakeTarget(ctx, st, reg, rt, env.To)
	if err != nil {
		return api.WakeOutcome{Reason: "recipient-unavailable", Detail: err.Error()}
	}
	if sessionID == "" {
		return api.WakeOutcome{Reason: "offline"}
	}
	text := intent.WakeText
	if text == "" {
		text = sweepWakeText(env)
	}
	// Coalesced wake (CW-20261117-0002): the turn lists the seat's other pending mail.
	text, commitDigest := rt.triage.withDigest(ctx, env, text)
	delegated = true
	out = attemptRecipientWake(ctx, st, reg, rt, env.ID, env.To, sessionID, text)
	if out.Delivered {
		rt.triage.woke(ctx, env)
		commitDigest()
	}
	return out
}

func attemptRecipientWake(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, id string, to messaging.Address, sessionID, text string) api.WakeOutcome {
	if _, tracked, err := st.DeliveryIDForMessage(ctx, id); err != nil || !tracked {
		return api.WakeOutcome{Reason: "untracked-delivery", Detail: fmt.Sprint(err)}
	}
	first, raw, err := st.AdmitRecipientWake(ctx, id)
	if err != nil {
		return api.WakeOutcome{Reason: "wake-admission-failed", Detail: err.Error()}
	}
	if !first {
		var prior api.WakeOutcome
		if raw != "" && json.Unmarshal([]byte(raw), &prior) == nil {
			return prior
		}
		return api.WakeOutcome{Reason: "wake-outcome-unknown"}
	}
	out := attemptWake(ctx, st, reg, rt, id, to, sessionID, text)
	encoded, _ := json.Marshal(out)
	if err := st.CompleteRecipientWake(ctx, id, string(encoded)); err != nil {
		// Keep the actual send result and make uncertain persistence visible.
		out.Detail = "wake outcome persistence failed: " + err.Error()
	}
	return out
}

// storedRecipientWake returns the wake outcome recorded for an admitted
// message: the first send's outcome, or the latest sweep retry's. An admission
// whose outcome was never written (a crash mid-attempt) answers
// wake-outcome-unknown, as attemptRecipientWake does. ok is false when the
// message was never admitted, or the lookup failed.
func storedRecipientWake(ctx context.Context, st *store.Store, id string) (api.WakeOutcome, bool) {
	if st == nil || id == "" {
		return api.WakeOutcome{}, false
	}
	raw, found, err := st.RecipientWakeOutcome(ctx, id)
	if err != nil || !found {
		return api.WakeOutcome{}, false
	}
	var prior api.WakeOutcome
	if raw == "" || json.Unmarshal([]byte(raw), &prior) != nil {
		return api.WakeOutcome{Reason: "wake-outcome-unknown"}, true
	}
	return prior, true
}
