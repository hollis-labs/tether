package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

const workerPermissionLineLimit = 64 << 10

type workerLogSpan struct {
	Offset int64 `json:"offset"`
	Length int   `json:"length"`
}

// The hub-owned session and launch select support; a producer cannot choose
// another session/provider by placing attribution in stdout JSON.
func (s *Service) workerPermissionSource(row *store.SessionRow) bool {
	plan, err := s.Store.GetLaunchPlan(row.ID)
	return err == nil && plan != nil && config.CanonicalRuntimeID(plan.ProviderBrand) == "codex" && config.ParseRuntimeKind(plan.RuntimeKind) == config.RuntimeKindJSONRPCStdio
}

// Raw protocol bytes remain in the existing session log. Persist only spans
// for a split line, so restart can rebuild stdout without mixing in stderr or
// copying params/secrets into a second database representation.
func (s *Service) workerPermissionOutput(row *store.SessionRow, f workerwire.Frame, log *os.File, offset int64) error {
	if f.Stream != "stdout" || !s.workerPermissionSource(row) {
		return nil
	}
	ctx := context.Background()
	state, err := s.Store.WorkerPermissionParser(ctx, row.ID)
	if err != nil {
		return err
	}
	if f.Seq <= state.Sequence {
		return nil
	}
	if f.Seq <= 0 || uint64(f.Seq) > math.MaxUint32 || len(f.Data) > math.MaxUint32 {
		return fmt.Errorf("worker permission source sequence exhausted")
	}
	var spans []workerLogSpan
	if err = json.Unmarshal([]byte(state.Spans), &spans); err != nil {
		return err
	}
	carry := []byte{}
	for _, span := range spans {
		if span.Offset < 0 || span.Length < 0 || len(carry)+span.Length > workerPermissionLineLimit {
			return fmt.Errorf("invalid worker permission log span")
		}
		raw := make([]byte, span.Length)
		if _, err = log.ReadAt(raw, span.Offset); err != nil {
			return err
		}
		carry = append(carry, raw...)
	}
	previous := len(carry)
	carry = append(carry, f.Data...)
	spans = append(spans, workerLogSpan{Offset: offset, Length: len(f.Data)})
	consumed := 0
	for {
		newline := bytes.IndexByte(carry, '\n')
		if newline < 0 {
			break
		}
		line := carry[:newline]
		consumed += newline + 1
		if !state.Discard && len(line) <= workerPermissionLineLimit {
			if consumed-previous <= 0 || consumed-previous > len(f.Data) {
				return fmt.Errorf("invalid worker observation boundary")
			}
			sequence := uint64(f.Seq)<<32 | uint64(consumed-previous) //nolint:gosec // G115: boundary checked positive and <= len(f.Data) <= MaxUint32 above
			if err = s.forwardWorkerPermission(row, line, sequence); err != nil {
				return err
			}
		}
		state.Discard = false
		carry = carry[newline+1:]
	}
	remaining := consumed
	for len(spans) > 0 && remaining >= spans[0].Length {
		remaining -= spans[0].Length
		spans = spans[1:]
	}
	if len(spans) > 0 {
		spans[0].Offset += int64(remaining)
		spans[0].Length -= remaining
	}
	if len(carry) > workerPermissionLineLimit || len(spans) > 4096 {
		spans = nil
		state.Discard = true
	}
	raw, err := json.Marshal(spans)
	if err != nil {
		return err
	}
	state.Spans, state.Sequence = string(raw), f.Seq
	return s.Store.SaveWorkerPermissionParser(ctx, row.ID, state)
}

func (s *Service) forwardWorkerPermission(row *store.SessionRow, line []byte, sequence uint64) error {
	var marker struct {
		Type  string              `json:"type"`
		Event runtimeevents.Event `json:"event"`
	}
	if err := json.Unmarshal(line, &marker); err != nil {
		return nil //nolint:nilerr // Ordinary stdout and malformed JSON carry no observation.
	}
	if marker.Type != "tether.runtime_event" {
		return nil
	}
	input := marker.Event
	if input.TurnID == "" || len(input.TurnID) > 512 {
		return nil
	}
	var data struct {
		RequestID json.RawMessage `json:"request_id"`
		Method    string          `json:"method"`
		Allowed   *bool           `json:"allowed"`
	}
	payload := map[string]any{}
	switch input.Kind {
	case runtimeevents.KindAgentPermissionRequested, runtimeevents.KindAgentPermissionResolved:
		if err := json.Unmarshal(input.Payload, &data); err != nil {
			return nil //nolint:nilerr // Malformed stdout observations are ignored, not retried.
		}
		if scalarRequestID(data.RequestID) == "" {
			return nil
		}
		// Only supported producer observations, not arbitrary native notification
		// names or prose suggesting that an approval might be waiting.
		switch data.Method {
		case "mcpServer/elicitation/request", "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/requestUserInput":
		default:
			return nil
		}
		payload["request_id"], payload["method"] = data.RequestID, data.Method
		if input.Kind == runtimeevents.KindAgentPermissionResolved && data.Allowed != nil {
			payload["allowed"] = *data.Allowed
		}
	case runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
	default:
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", row.ID, sequence, input.Kind)))
	id := "evt_" + hex.EncodeToString(hash[:16])
	ev := runtimeevents.Event{SchemaVersion: runtimeevents.SchemaVersion, ID: id, Kind: input.Kind, SessionID: row.ID, TurnID: input.TurnID, Sequence: sequence, Time: time.Now().UTC(), App: "tether-worker", Payload: raw, Process: runtimeevents.Process{Provider: "codex"}, Source: runtimeevents.Source{Channel: runtimeevents.ChannelJSONRPC, Confidence: runtimeevents.ConfidenceExact}, Scope: &runtimeevents.Scope{WorkerID: row.WorkerID, TaskID: row.TaskID.String, ProjectID: row.ProjectID, WorkstreamID: row.WorkstreamID.String, LaunchID: row.LaunchID, LogicalAgentID: row.LogicalAgentID}}
	origin, err := derivedRuntimeOrigin(ev)
	if err != nil {
		return err
	}
	if err = s.recordRuntimeRequest(*row, ev, origin); err != nil {
		return err
	}
	observed := origin.apply(events.Event{EventID: id, Scope: events.ScopeSession, SessionID: row.ID, LogicalAgentID: row.LogicalAgentID, Kind: string(ev.Kind), PayloadJSON: string(raw)})
	return s.Bus.Publish(context.Background(), observed)
}
