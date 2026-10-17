package workerruntime

import "encoding/json"

func workerTurnComplete(provider string, line []byte) bool {
	var frame struct {
		Type   string `json:"type"`
		Event  string `json:"event"`
		Method string `json:"method"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return false
	}
	switch provider {
	case "claude":
		return frame.Type == "result"
	case "antigravity":
		return frame.Event == "result"
	case "codex":
		return frame.Method == "turn/completed" || frame.Type == "turn.completed" || frame.Type == "turn.failed"
	default:
		return true
	}
}
