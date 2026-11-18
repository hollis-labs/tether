// Package claudetranscript reads a Claude Code session transcript (the JSONL file under ~/.claude/projects/<project>/
// <session-id>.jsonl) as a sequence of turns, and turns each one into the frames the claudejson decoder
// (chatstream/adapter/claudejson) reads, so a transcript becomes chatstream events the same way `--output-format
// stream-json` does, minus the deltas.
//
// It is generic to Claude transcripts: it uses only what the transcript itself records (promptId on user lines,
// message.id on assistant lines, the turn_duration and stop_hook_summary system lines, the interrupt marker) and knows
// nothing about Tether or its hooks. It depends only on the standard library and libs ui-go, so it can move to
// libs/ui/ui-go/chatstream/adapter as it is.
//
// What a transcript says, as of Claude Code 2.1.327 (recorded in the claudepty testdata):
//
//   - A turn starts with a user line whose promptId is new. Its content is the user's input (a string, or text blocks).
//     promptSource ("typed", "system") and turnOrigin ("human", "system", "peer", "task_notification") say where it
//     came from. Further user lines with the same promptId are tool results, or text the CLI injected (isMeta).
//   - Each assistant line holds one content block. The blocks of one API message share message.id, and Claude Code
//     writes a tool's result line right after its tool_use line, before the message's next block, so one message can be
//     split by tool results. The Mapper holds tool results back until the message they interrupt is complete.
//   - A completed turn ends with system/stop_hook_summary then system/turn_duration. An interrupted one (Esc) ends with
//     a user text line "[Request interrupted by user]" (or "... for tool use]") and no summary.
//   - Input typed while a turn runs is folded into it: an attachment of type queued_command carries the text.
//   - Lines are written in batches. The last assistant line of a turn can land after the Stop hook has fired.
package claudetranscript

import (
	"encoding/json"
	"strings"
)

// Line is one transcript line, as far as turn mapping cares.
type Line struct {
	Kind      LineKind
	PromptID  string // user lines
	MessageID string // assistant lines
	Text      string // LineUserText: the input; LineQueued: the queued text
	Source    string // promptSource
	Origin    string // turnOrigin
	Raw       []byte
	// StopReason and Usage of an assistant line's message (repeated on every line of the message).
	StopReason string
	Usage      *Usage
}

// Usage is an assistant message's token counts, as the transcript records them.
type Usage struct {
	Input       int `json:"input_tokens"`
	Output      int `json:"output_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
}

// LineKind classifies a transcript line.
type LineKind int

// The line kinds.
const (
	// LineOther is everything that is not conversation: attachments, api-request records, titles, snapshots, sidechain
	// (subagent) lines.
	LineOther LineKind = iota
	// LineUserText is a user line with text content: a new prompt, an injected meta message, or an interrupt marker.
	LineUserText
	// LineToolResult is a user line carrying tool_result blocks.
	LineToolResult
	// LineAssistant is an assistant line (one content block).
	LineAssistant
	// LineTurnEnd is system/stop_hook_summary or system/turn_duration.
	LineTurnEnd
	// LineQueued is an attachment of type queued_command: input that arrived while the turn ran.
	LineQueued
)

// interruptPrefix starts the user line Claude Code writes when a turn is interrupted.
const interruptPrefix = "[Request interrupted by user"

// Interrupted reports whether l is the interrupt marker.
func (l Line) Interrupted() bool {
	return l.Kind == LineUserText && strings.HasPrefix(l.Text, interruptPrefix)
}

// Parse classifies one transcript line. A line that is not JSON is LineOther.
func Parse(raw []byte) Line {
	var w struct {
		Type          string `json:"type"`
		Subtype       string `json:"subtype"`
		PromptID      string `json:"promptId"`
		IsSidechain   bool   `json:"isSidechain"`
		IsCompact     bool   `json:"isCompactSummary"`
		TranscriptVis bool   `json:"isVisibleInTranscriptOnly"`
		PromptSource  string `json:"promptSource"`
		TurnOrigin    string `json:"turnOrigin"`
		Message       struct {
			ID         string          `json:"id"`
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
			Usage      *Usage          `json:"usage"`
		} `json:"message"`
		Attachment struct {
			Type   string `json:"type"`
			Prompt string `json:"prompt"`
		} `json:"attachment"`
	}
	l := Line{Raw: raw}
	if json.Unmarshal(raw, &w) != nil || w.IsSidechain {
		return l
	}
	switch w.Type {
	case "user":
		if w.IsCompact || w.TranscriptVis {
			return l
		}
		l.PromptID, l.Source, l.Origin = w.PromptID, w.PromptSource, w.TurnOrigin
		text, toolResult := userContent(w.Message.Content)
		switch {
		case toolResult:
			l.Kind = LineToolResult
		case text != "" || len(w.Message.Content) > 0:
			l.Kind, l.Text = LineUserText, text
		}
	case "assistant":
		l.Kind, l.MessageID, l.StopReason, l.Usage = LineAssistant, w.Message.ID, w.Message.StopReason, w.Message.Usage
	case "system":
		if w.Subtype == "turn_duration" || w.Subtype == "stop_hook_summary" {
			l.Kind = LineTurnEnd
		}
	case "attachment":
		if w.Attachment.Type == "queued_command" {
			l.Kind, l.Text = LineQueued, w.Attachment.Prompt
		}
	}
	return l
}

// userContent is a user message's text and whether it carries tool results.
func userContent(raw json.RawMessage) (text string, toolResult bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, false
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			toolResult = true
		case "text":
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), toolResult
}
