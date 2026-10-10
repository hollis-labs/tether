package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/substrate/mesh/messaging"
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
		out := recipientWake(ctx, s.Store, s.Registry, s.runtimeSeam(), env)
		raw, _ := json.Marshal(out)
		return string(raw)
	})
}

func recipientWake(ctx context.Context, st *store.Store, reg *registry.Service, rt wakeRuntime, env messaging.Envelope) api.WakeOutcome {
	intent, err := wakeintent.Read(env)
	if err != nil {
		return api.WakeOutcome{Reason: "invalid-wake-intent", Detail: err.Error()}
	}
	if intent.NoWake {
		return api.WakeOutcome{Reason: "no-wake"}
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
	return attemptRecipientWake(ctx, st, reg, rt, env.ID, env.To, sessionID, text)
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
