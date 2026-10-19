package workerruntime

import (
	"encoding/json"
	"io"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

// Observations use the existing stdout journal/worker output transport. They
// cannot answer a request and carry no params, tool arguments or answers.
func writeProviderObservation(out io.Writer, kind runtimeevents.EventKind, turn string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Type  string              `json:"type"`
		Event runtimeevents.Event `json:"event"`
	}{"tether.runtime_event", runtimeevents.Event{SchemaVersion: runtimeevents.SchemaVersion, Kind: kind, TurnID: turn, Time: time.Now().UTC(), Payload: raw}})
}

func workerPermissionRequest(method string, id, params json.RawMessage) (string, map[string]any) {
	switch method {
	case "mcpServer/elicitation/request", "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/requestUserInput":
	default:
		return "", nil
	}
	if len(id) == 0 || len(id) > 512 {
		return "", nil
	}
	var scalar any
	if json.Unmarshal(id, &scalar) != nil {
		return "", nil
	}
	switch v := scalar.(type) {
	case string:
		if v == "" {
			return "", nil
		}
	case float64:
		var exact json.Number
		if json.Unmarshal(id, &exact) != nil {
			return "", nil
		}
		if _, err := exact.Int64(); err != nil {
			return "", nil
		}
	default:
		return "", nil
	}
	var scope struct {
		TurnID string `json:"turnId"`
	}
	if json.Unmarshal(params, &scope) != nil || scope.TurnID == "" || len(scope.TurnID) > 512 {
		return "", nil
	}
	return scope.TurnID, map[string]any{"request_id": id, "method": method}
}
