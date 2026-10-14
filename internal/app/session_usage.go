package app

import (
	"encoding/json"
	"log"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/store"
)

// meterTurn folds a terminal turn event into the session's usage meter
// (CW-20261014-0042). Called with o.mu held, after the finished-turn dedupe.
// Usage is whatever the runtime put on the event: native runtimes use the go-llm-types
// field names, ACP passes the agent's own object through, so both spellings
// are read. A terminal event with no usage object counts the turn and leaves
// the token columns alone, which marks the session as unmetered rather than
// guessing a figure.
func (o *sessionTurnOutput) meterTurn(ev runtimeevents.Event) {
	if o.service == nil || o.service.Store == nil {
		return
	}
	switch ev.Kind {
	case runtimeevents.KindTurnStarted:
		if ev.TurnID != "" {
			if o.turnStartedAt == nil {
				o.turnStartedAt = map[string]time.Time{}
			}
			o.turnStartedAt[ev.TurnID] = ev.Time
		}
		return
	case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
	default:
		return
	}
	u := store.TurnUsage{Failed: ev.Kind == runtimeevents.KindTurnFailed}
	if started, ok := o.turnStartedAt[ev.TurnID]; ok {
		delete(o.turnStartedAt, ev.TurnID)
		if d := ev.Time.Sub(started); d > 0 {
			u.Duration = d
		}
	}
	var payload struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(ev.Payload, &payload) == nil && len(payload.Usage) > 0 {
		in, okIn := usageNumber(payload.Usage, "InputTokens", "input_tokens", "inputTokens")
		out, okOut := usageNumber(payload.Usage, "OutputTokens", "output_tokens", "outputTokens")
		cost, _ := usageNumber(payload.Usage, "CostUSD", "cost_usd", "costUsd")
		if okIn || okOut {
			u.Reported = true
			u.InputTokens, u.OutputTokens, u.CostUSD = int64(in), int64(out), cost
		}
	}
	if err := o.service.Store.AddSessionTurnUsage(o.row.ID, u); err != nil {
		log.Printf("session usage %s: %v", o.row.ID, err)
	}
}

func usageNumber(m map[string]json.RawMessage, keys ...string) (float64, bool) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil && f >= 0 {
			return f, true
		}
	}
	return 0, false
}
