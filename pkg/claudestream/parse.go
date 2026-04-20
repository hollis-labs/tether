package claudestream

import (
	"encoding/json"
	"fmt"
)

// Parse translates a single line of claude stream-json output into
// zero or more Events. Empty lines return (nil, nil). Informational
// noise (rate_limit_event, system events without session_id) also
// returns (nil, nil) — consumers don't need to filter them.
//
// Invalid JSON returns a descriptive error and no events so the
// caller can log + continue to the next line rather than abort the
// whole stream on one bad frame.
func Parse(line []byte) ([]Event, error) {
	if len(line) == 0 {
		return nil, nil
	}

	var env wireEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("claudestream: parse envelope: %w", err)
	}

	switch env.Type {
	case "assistant":
		return parseAssistant(line)
	case "result":
		return parseResult(line)
	case "error":
		return parseError(line)
	case "system":
		return parseSystem(line)
	case "rate_limit_event":
		return nil, nil
	default:
		// Forward-compat: silently skip unknown event types so a new
		// claude release introducing a new kind doesn't crash us.
		return nil, nil
	}
}

func parseAssistant(line []byte) ([]Event, error) {
	var ev wireAssistantEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("claudestream: parse assistant: %w", err)
	}
	var out []Event
	for _, block := range ev.Message.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				out = append(out, Event{Kind: KindDelta, Text: block.Text})
			}
		case "tool_use":
			input := make(map[string]any)
			if len(block.Input) > 0 {
				_ = json.Unmarshal(block.Input, &input)
			}
			out = append(out, Event{
				Kind: KindToolUse,
				ToolUse: &ToolUseBlock{
					ID:    block.ID,
					Name:  block.Name,
					Input: input,
				},
			})
			// tool_result blocks are the claude CLI's internal tool
			// loop echo; consumers render tool_use + observe effects
			// elsewhere. Skip.
		}
	}
	return out, nil
}

func parseResult(line []byte) ([]Event, error) {
	var ev wireResultEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("claudestream: parse result: %w", err)
	}
	if ev.IsError || ev.Subtype == "error" {
		return []Event{{Kind: KindError, ErrorMsg: ev.Result}}, nil
	}
	var out []Event
	if ev.Usage != nil {
		stop := ev.StopReason
		if stop == "" {
			stop = "end_turn"
		}
		out = append(out, Event{
			Kind: KindUsage,
			Usage: &Usage{
				InputTokens:         ev.Usage.InputTokens,
				OutputTokens:        ev.Usage.OutputTokens,
				CacheCreationTokens: ev.Usage.CacheCreationInputTokens,
				CacheReadTokens:     ev.Usage.CacheReadInputTokens,
				StopReason:          stop,
			},
		})
	}
	out = append(out, Event{Kind: KindDone})
	return out, nil
}

func parseSystem(line []byte) ([]Event, error) {
	var ev wireSystemEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("claudestream: parse system: %w", err)
	}
	if ev.Subtype == "init" && ev.SessionID != "" {
		return []Event{{Kind: KindSessionID, SessionID: ev.SessionID}}, nil
	}
	return nil, nil
}

func parseError(line []byte) ([]Event, error) {
	var ev wireErrorEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("claudestream: parse error: %w", err)
	}
	return []Event{{Kind: KindError, ErrorMsg: ev.Error.Message}}, nil
}
