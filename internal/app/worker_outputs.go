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

// Uses the durable turn-output/routing writer for native worker result frames.
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
			Type     string          `json:"type"`
			Raw      json.RawMessage `json:"result"`
			Event    string          `json:"event"`
			Provider string          `json:"provider"`
			UUID     string          `json:"uuid"`
			IsError  bool            `json:"is_error"`
		}
		if json.Unmarshal(line, &result) != nil || (result.Type != "result" && result.Event != "result") {
			continue
		}
		var text string
		if result.Event == "result" {
			var agy struct {
				Response string `json:"response"`
				Status   string `json:"status"`
				Error    string `json:"error"`
			}
			if json.Unmarshal(result.Raw, &agy) != nil {
				continue
			}
			text, result.IsError, result.Provider = agy.Response, agy.Status != "SUCCESS", "antigravity"
			if result.IsError && text == "" {
				text = agy.Error
			}
		} else if json.Unmarshal(result.Raw, &text) != nil {
			continue
		}
		if result.Provider == "" {
			result.Provider = "claude"
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
		job := &turnOutputWrite{row: *row, route: route, providerResultID: result.UUID, result: turnoutput.Output{SessionID: row.ID, TurnID: turn, Kind: kind, Text: text, Runtime: result.Provider, StopReason: "end_turn", Confidence: turnoutput.ConfidenceExact}}
		if err := job.persist(context.Background(), s); err != nil {
			return err
		}
	}
	s.workerCarries[row.ID] = carry
	return nil
}
