package claudestream

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	sentinelOpen  = "[[UI_PROMPT:"
	sentinelClose = "]]"
)

// extractUIPromptSentinel scans text for a [[UI_PROMPT:{...}]] sentinel
// emitted by agents that use the text-based (non-MCP) ui_prompt convention.
// Returns the remaining text (sentinel stripped) and the parsed descriptor.
// Returns ("", nil) when no valid sentinel is present.
// Only the first sentinel per text block is processed.
func extractUIPromptSentinel(text string) (string, *UIPromptDescriptor) {
	start := strings.Index(text, sentinelOpen)
	if start == -1 {
		return "", nil
	}
	rest := text[start+len(sentinelOpen):]
	end := strings.Index(rest, sentinelClose)
	if end == -1 {
		return "", nil
	}
	var desc UIPromptDescriptor
	if err := json.Unmarshal([]byte(rest[:end]), &desc); err != nil {
		return "", nil
	}
	if desc.Kind == "" {
		return "", nil // require at least kind to be present
	}
	before := strings.TrimRight(text[:start], " \t")
	after := strings.TrimLeft(rest[end+len(sentinelClose):], " \t\n")
	remaining := before
	if before != "" && after != "" {
		remaining = before + "\n" + after
	} else if after != "" {
		remaining = after
	}
	return remaining, &desc
}

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
				if remaining, desc := extractUIPromptSentinel(block.Text); desc != nil {
					if remaining != "" {
						out = append(out, Event{Kind: KindDelta, Text: remaining})
					}
					out = append(out, Event{Kind: KindUIPrompt, UIPrompt: desc})
				} else {
					out = append(out, Event{Kind: KindDelta, Text: block.Text})
				}
			}
		case "tool_use":
			if block.Name == "ui_prompt" {
				// Intercept: parse the input as a UIPromptDescriptor and emit
				// KindUIPrompt. The tool_use is NOT forwarded to consumers as
				// KindToolUse — the panel owns the interaction.
				// Fall back to KindToolUse on malformed input so the agent
				// can observe the failure rather than silently losing the call.
				var desc UIPromptDescriptor
				if len(block.Input) > 0 {
					if err := json.Unmarshal(block.Input, &desc); err != nil || desc.Kind == "" {
						// Malformed — emit as normal tool_use so nothing is lost.
						input := make(map[string]any)
						_ = json.Unmarshal(block.Input, &input)
						out = append(out, Event{Kind: KindToolUse, ToolUse: &ToolUseBlock{ID: block.ID, Name: block.Name, Input: input}})
						continue
					}
				}
				desc.ToolUseID = block.ID
				out = append(out, Event{
					Kind:     KindUIPrompt,
					UIPrompt: &desc,
				})
				continue
			}
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
