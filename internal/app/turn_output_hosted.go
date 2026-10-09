package app

import (
	"context"
	"errors"
	"slices"

	"github.com/hollis-labs/substrate/harness/adapters/turnoutput"
)

// persistHostedTurnOutput is the producer half of hosted delivery. The caller
// retains its native inbox until its own receipt transaction verifies this
// acceptance. A nil error means actual stage/event persistence, never a send ACK
// or admission to a volatile retry worker. No callback-fed reducer runs again.
func (s *Service) persistHostedTurnOutput(ctx context.Context, sessionID string, result turnoutput.Output, providerResultID string) (messageID, outputID string, err error) {
	if result.TurnID == "" || providerResultID == "" || s.Store == nil || s.Bus == nil {
		return "", "", errors.New("hosted output requires durable source identity and producer wiring")
	}
	row, err := s.Store.GetSessionContext(ctx, sessionID)
	if err != nil {
		return "", "", err
	}
	route, err := s.Store.SessionRoute(ctx, sessionID)
	if err != nil {
		return "", "", err
	}
	job := &turnOutputWrite{row: *row, route: route, result: result, providerResultID: providerResultID}
	var output *sessionTurnOutput
	if value, ok := s.turnOutputs.Load(sessionID); ok {
		output = value.(*sessionTurnOutput)
		output.mu.Lock()
		if !slices.Contains(output.finishedTurns, result.TurnID) {
			output.bindTurn(result.TurnID)
		}
		job.freshConversation = output.freshConversationTurn != "" && output.freshConversationTurn == output.turnID
		output.mu.Unlock()
	}
	r := &s.outputRetries
	r.mu.Lock()
	err = job.journal(s)
	if err == nil && r.active[job.journalID] {
		err = errors.New("hosted output persistence already in progress")
	}
	if err == nil {
		if r.active == nil {
			r.active = make(map[string]bool)
		}
		r.active[job.journalID] = true
	}
	r.mu.Unlock()
	if err != nil {
		return "", job.journalID, err
	}
	defer func() { r.mu.Lock(); delete(r.active, job.journalID); r.mu.Unlock() }()
	if err := job.persist(ctx, s); err != nil {
		return job.messageID, job.journalID, err
	}
	if output != nil && (result.Kind == turnoutput.KindFinal || result.Kind == turnoutput.KindFailure || result.Kind == turnoutput.KindTerminal) {
		output.mu.Lock()
		output.completeTurn(result, false)
		output.mu.Unlock()
	}
	return job.messageID, job.journalID, nil
}
