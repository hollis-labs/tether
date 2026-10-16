package app

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// usageSchema is the payload schema id (session_usage@1, harness/core v0.12.0).
const usageSchema = "session_usage@1"

// sessionUsagePayload is the session_usage@1 payload. Totals are cumulative
// since the session started and only grow. turn_id (an extra field the schema
// allows) names the turn that just finished; the envelope has no turn column
// yet.
type sessionUsagePayload struct {
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Turns        int64  `json:"turns"`
	Final        bool   `json:"final,omitempty"`
	Source       string `json:"source"`
	TurnID       string `json:"turn_id,omitempty"`
}

// publishSessionUsage reports the session's cumulative usage from its meter.
// It reports nothing for a session whose runtime never reported usage: zero
// totals would claim a measurement nobody made, and Tether does not estimate.
// Turns counts every finished turn, including ones that reported no usage, so
// a consumer can see the meter is partial.
func publishSessionUsage(bus events.Publisher, db *store.Store, sessionID, logicalAgentID, turnID string, final bool) {
	if bus == nil || db == nil || sessionID == "" {
		return
	}
	u, err := db.GetSessionUsage(sessionID)
	if err != nil {
		log.Printf("session usage event %s: %v", sessionID, err)
		return
	}
	if u.UsageTurns == 0 {
		return
	}
	data, err := json.Marshal(sessionUsagePayload{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, Turns: u.Turns, Final: final, Source: "provider", TurnID: turnID})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := events.Event{Scope: events.ScopeSession, SessionID: sessionID, LogicalAgentID: logicalAgentID, Kind: events.KindSessionUsage, PayloadJSON: string(data)}
	e.Envelope.Class = events.Class{Category: "activity", PayloadSchema: usageSchema}
	if err = bus.Publish(ctx, e); err != nil {
		log.Printf("session usage event %s: %v", sessionID, err)
	}
}

// publishFinalUsageIfTerminal sends the closing report (final=true) when a
// state change ends the session. Several paths can record terminal changes for
// one session (a stop followed by the provider's exit, a reaper move), so it is
// at most once per session: a final report already on the log suppresses
// another.
func publishFinalUsageIfTerminal(bus events.Publisher, db *store.Store, sessionID, logicalAgentID, to string) {
	if !session.State(to).Terminal() || db == nil {
		return
	}
	var already bool
	err := db.DB().QueryRow(`SELECT EXISTS(SELECT 1 FROM events WHERE session_id=? AND kind=? AND json_extract(payload_json,'$.final')=1)`, sessionID, events.KindSessionUsage).Scan(&already)
	if err != nil || already {
		return
	}
	publishSessionUsage(bus, db, sessionID, logicalAgentID, "", true)
}
