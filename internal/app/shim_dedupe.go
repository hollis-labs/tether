//go:build !windows

package app

import (
	"encoding/json"

	llmtypes "github.com/hollis-labs/go-llm-types"
	gop "github.com/hollis-labs/go-providers/provider"
	pevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// A result's own UUID is carried into the durable published output. Replay
// suppresses only an exact match to that identity, never matching by text.
type shimClaudeAdapter struct {
	*gop.ClaudeAdapter
	service   *Service
	sessionID string
	duplicate bool
}

func (a *shimClaudeAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	var result struct {
		Type string `json:"type"`
		UUID string `json:"uuid"`
	}
	a.duplicate = false
	identity := ""
	if json.Unmarshal(line, &result) == nil && result.Type == "result" {
		identity = result.UUID
	}
	if identity != "" {
		rows, err := a.service.Store.QueryEvents(store.EventFilter{SessionID: a.sessionID, Kinds: []string{events.KindSessionTurnOutput}, Limit: 1})
		if err == nil && len(rows) == 1 {
			var published events.TurnOutputEvent
			if json.Unmarshal([]byte(rows[0].PayloadJSON), &published) == nil {
				a.duplicate = published.ProviderResultID == identity
			}
		}
	}
	if a.duplicate {
		return nil, nil
	}
	if value, ok := a.service.turnOutputs.Load(a.sessionID); ok {
		output := value.(*sessionTurnOutput)
		output.mu.Lock()
		output.providerResultID = identity
		output.mu.Unlock()
	}
	return a.ClaudeAdapter.ParseLine(line)
}

func (a *shimClaudeAdapter) ParseLineEvents(line []byte) ([]pevents.Event, error) {
	if a.duplicate {
		return nil, nil
	}
	return a.ClaudeAdapter.ParseLineEvents(line)
}
