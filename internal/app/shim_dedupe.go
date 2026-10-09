//go:build !windows

package app

import (
	"encoding/json"

	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	pevents "github.com/hollis-labs/substrate/harness/adapters/provider/events"
	llmtypes "github.com/hollis-labs/substrate/llm-core/llmtypes"
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
		ctx, cancel := a.service.outputPersistenceContext()
		published, err := a.service.Store.HasPublishedProviderResult(ctx, a.sessionID, identity)
		cancel()
		if err == nil {
			a.duplicate = published
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
