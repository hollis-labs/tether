package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/libs/harness/core/adapters/turnoutput"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// Uses the existing durable turn-output/routing writer for Claude result frames.
// Called under workerIngestMu; raw journal bytes remain the session log.
func (s *Service) workerTurnOutput(row *store.SessionRow, f workerwire.Frame) error {
	if f.Stream != "stdout" || s.Bus == nil {
		return nil
	}
	if s.workerCarries == nil {
		s.workerCarries = map[string][]byte{}
	}
	carry := append(s.workerCarries[row.ID], f.Data...)
	for {
		at := bytes.IndexByte(carry, '\n')
		if at < 0 {
			break
		}
		line := carry[:at]
		carry = carry[at+1:]
		var result struct {
			Type    string `json:"type"`
			Text    string `json:"result"`
			UUID    string `json:"uuid"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(line, &result) != nil || result.Type != "result" {
			continue
		}
		kind := turnoutput.KindFinal
		if result.IsError {
			kind = turnoutput.KindFailure
		}
		turn := result.UUID
		if turn == "" {
			turn = fmt.Sprintf("worker:%s:%d", row.ID, f.Seq)
		}
		route, err := s.Store.SessionRoute(context.Background(), row.ID)
		if err != nil {
			return err
		}
		job := &turnOutputWrite{row: *row, route: route, providerResultID: result.UUID, result: turnoutput.Output{SessionID: row.ID, TurnID: turn, Kind: kind, Text: result.Text, Runtime: "claude", StopReason: "end_turn", Confidence: turnoutput.ConfidenceExact}}
		if err := job.persist(context.Background(), s); err != nil {
			return err
		}
	}
	s.workerCarries[row.ID] = carry
	return nil
}
